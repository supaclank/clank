package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HeaderDeviceID matches pkg/sync.HeaderDeviceID. Forwarded from the
// laptop's request to clank-sync so ownership checks fire correctly.
// Pinned here to avoid a package cycle (gateway should not import sync).
const HeaderDeviceID = "X-Clank-Device-Id"

// migrateRequest is the body of POST /v1/migrate/worktrees/{id}.
type migrateRequest struct {
	Direction string `json:"direction"` // "to_remote" only in P3
	Confirm   bool   `json:"confirm"`
}

// migrateResponse is returned on a successful migration.
type migrateResponse struct {
	WorktreeID   string `json:"worktree_id"`
	NewOwnerKind string `json:"new_owner_kind"`
	NewOwnerID   string `json:"new_owner_id"`
	CheckpointID string `json:"checkpoint_id"`
}

// handleMigrateWorktree orchestrates a worktree's migration from the
// laptop to a sandbox sprite. The flow (per the architecture plan):
//
//  1. Read the worktree from clank-sync; reject if not laptop-owned by
//     the calling device.
//  2. Pre-check that there's an uploaded checkpoint to migrate.
//  3. Wake the sprite via Provisioner.EnsureHost.
//  4. Download the latest checkpoint's bundles from object storage via
//     presigned GET URLs minted by clank-sync.
//  5. Push the checkpoint to the sprite as a multipart /sync/apply
//     request — manifest + headCommit + incremental.
//  6. Atomic ownership transfer via clank-sync. Loser of any race
//     gets 409 and surfaces it to the caller.
//
// Bundle bytes pass through the gateway's memory but are never
// persisted there. With encryption (P6), the gateway sees ciphertext
// only and cannot tamper because the sprite verifies the manifest
// signature.
func (g *Gateway) handleMigrateWorktree(w http.ResponseWriter, r *http.Request) {
	if _, err := g.cfg.Auth.Verify(r); err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="clank"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Auth gates the deployment-state signal so an unauthenticated
	// caller can't probe whether migration is wired up.
	if g.cfg.SyncBaseURL == "" {
		http.Error(w, "migration not configured (SyncBaseURL unset)", http.StatusServiceUnavailable)
		return
	}
	userID := g.cfg.ResolveUserID(r)
	if userID == "" {
		http.Error(w, "no user identity", http.StatusUnauthorized)
		return
	}
	deviceID := r.Header.Get(HeaderDeviceID)
	if deviceID == "" {
		http.Error(w, "missing "+HeaderDeviceID, http.StatusBadRequest)
		return
	}
	worktreeID := r.PathValue("id")
	if worktreeID == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}

	var req migrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Direction != "to_remote" && req.Direction != "to_local" {
		http.Error(w, "direction must be to_remote or to_local", http.StatusBadRequest)
		return
	}
	if !req.Confirm {
		http.Error(w, "confirm must be true", http.StatusBadRequest)
		return
	}

	mc := g.migrationClient(deviceID)
	wt, err := mc.getWorktree(r.Context(), worktreeID)
	if err != nil {
		mc.respond(w, "read worktree", err)
		return
	}

	switch req.Direction {
	case "to_remote":
		g.migrateToRemote(w, r, mc, wt, userID, deviceID)
	case "to_local":
		g.migrateToLocal(w, r, mc, wt, deviceID)
	}
}

// migrateToRemote is the §D happy path: pre-checked → wake → push →
// atomic transfer. Idempotent for re-runs against the same sprite —
// if the worktree is already owned by some sprite for this user, we
// no-op rather than 403, which means a user who calls migrate twice
// gets the same answer the second time.
func (g *Gateway) migrateToRemote(w http.ResponseWriter, r *http.Request, mc *migrationClient, wt worktreeView, userID, deviceID string) {
	if wt.OwnerKind == "remote" {
		// Already sprite-owned. Treat as no-op success.
		writeJSON(w, http.StatusOK, migrateResponse{
			WorktreeID:   wt.ID,
			NewOwnerKind: wt.OwnerKind,
			NewOwnerID:   wt.OwnerID,
			CheckpointID: wt.LatestSyncedCheckpoint,
		})
		return
	}
	if wt.OwnerKind != "local" || wt.OwnerID != deviceID {
		http.Error(w, "caller is not the current laptop owner of this worktree", http.StatusForbidden)
		return
	}
	if wt.LatestSyncedCheckpoint == "" {
		http.Error(w, "worktree has no synced checkpoint; push one first", http.StatusConflict)
		return
	}

	hostRef, err := g.cfg.Provisioner.EnsureHost(r.Context(), userID)
	if err != nil {
		g.log.Printf("gateway migrate: EnsureHost(%s): %v", userID, err)
		http.Error(w, "ensure sprite: "+err.Error(), http.StatusBadGateway)
		return
	}

	ck, err := mc.downloadCheckpointURLs(r.Context(), wt.LatestSyncedCheckpoint)
	if err != nil {
		mc.respond(w, "download checkpoint URLs", err)
		return
	}
	manifestBytes, err := mc.fetchBlob(r.Context(), ck.ManifestGetURL)
	if err != nil {
		mc.respond(w, "fetch manifest", err)
		return
	}
	headBytes, err := mc.fetchBlob(r.Context(), ck.HeadCommitGetURL)
	if err != nil {
		mc.respond(w, "fetch headCommit bundle", err)
		return
	}
	incrBytes, err := mc.fetchBlob(r.Context(), ck.IncrementalURL)
	if err != nil {
		mc.respond(w, "fetch incremental bundle", err)
		return
	}

	// Use the worktree ID as the sprite-side directory name so
	// session-create's WorktreeID lookup hits ~/work/<id>/.
	if err := mc.applyToSprite(r.Context(), hostRef.URL, hostRef.Transport, hostRef.AuthToken, wt.ID, manifestBytes, headBytes, incrBytes); err != nil {
		g.log.Printf("gateway migrate: apply to sprite: %v", err)
		http.Error(w, "apply to sprite: "+err.Error(), http.StatusBadGateway)
		return
	}

	updated, err := mc.transferOwnership(r.Context(), wt.ID, "remote", hostRef.HostID, deviceID)
	if err != nil {
		mc.respond(w, "transfer ownership", err)
		return
	}

	writeJSON(w, http.StatusOK, migrateResponse{
		WorktreeID:   updated.ID,
		NewOwnerKind: updated.OwnerKind,
		NewOwnerID:   updated.OwnerID,
		CheckpointID: wt.LatestSyncedCheckpoint,
	})
}

// migrateToLocal reclaims ownership from a sprite. Implements the
// "Keep local" semantic: ownership transfers, sprite-side changes are
// abandoned. The "Pull from sandbox" variant — download the sprite's
// latest checkpoint, apply it locally, then transfer — needs sprite-
// side push (P4) and isn't here yet.
//
// Idempotent: if already laptop-owned by this device, no-op success.
func (g *Gateway) migrateToLocal(w http.ResponseWriter, r *http.Request, mc *migrationClient, wt worktreeView, deviceID string) {
	if wt.OwnerKind == "local" && wt.OwnerID == deviceID {
		writeJSON(w, http.StatusOK, migrateResponse{
			WorktreeID:   wt.ID,
			NewOwnerKind: wt.OwnerKind,
			NewOwnerID:   wt.OwnerID,
			CheckpointID: wt.LatestSyncedCheckpoint,
		})
		return
	}
	if wt.OwnerKind != "remote" {
		http.Error(w, "worktree is not currently sprite-owned", http.StatusForbidden)
		return
	}

	updated, err := mc.transferOwnership(r.Context(), wt.ID, "local", deviceID, wt.OwnerID)
	if err != nil {
		mc.respond(w, "transfer ownership", err)
		return
	}

	writeJSON(w, http.StatusOK, migrateResponse{
		WorktreeID:   updated.ID,
		NewOwnerKind: updated.OwnerKind,
		NewOwnerID:   updated.OwnerID,
		CheckpointID: wt.LatestSyncedCheckpoint,
	})
}

// migrationClient bundles the per-request HTTP client used to talk to
// clank-sync and the sprite. DeviceID is forwarded as
// X-Clank-Device-Id so clank-sync's ownership checks pass.
type migrationClient struct {
	syncURL  string
	deviceID string
	client   *http.Client
}

func (g *Gateway) migrationClient(deviceID string) *migrationClient {
	cli := g.cfg.SyncHTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 5 * time.Minute}
	}
	return &migrationClient{
		syncURL:  strings.TrimRight(g.cfg.SyncBaseURL, "/"),
		deviceID: deviceID,
		client:   cli,
	}
}

// worktreeView mirrors the JSON shape clank-sync emits for worktrees.
// Kept here (rather than imported from pkg/sync) to avoid the gateway
// taking a build dependency on sync types.
type worktreeView struct {
	ID                     string `json:"id"`
	UserID                 string `json:"user_id"`
	DisplayName            string `json:"display_name"`
	OwnerKind              string `json:"owner_kind"`
	OwnerID                string `json:"owner_id"`
	LatestSyncedCheckpoint string `json:"latest_synced_checkpoint"`
}

func (m *migrationClient) getWorktree(ctx context.Context, id string) (worktreeView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.syncURL+"/v1/worktrees/"+url.PathEscape(id), nil)
	if err != nil {
		return worktreeView{}, err
	}
	m.attachHeaders(req)
	resp, err := m.client.Do(req)
	if err != nil {
		return worktreeView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return worktreeView{}, &syncErr{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var out worktreeView
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return worktreeView{}, fmt.Errorf("decode worktree: %w", err)
	}
	return out, nil
}

type checkpointDownloadView struct {
	CheckpointID     string `json:"checkpoint_id"`
	HeadCommitGetURL string `json:"head_commit_get_url"`
	IncrementalURL   string `json:"incremental_get_url"`
	ManifestGetURL   string `json:"manifest_get_url"`
}

func (m *migrationClient) downloadCheckpointURLs(ctx context.Context, checkpointID string) (checkpointDownloadView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.syncURL+"/v1/checkpoints/"+url.PathEscape(checkpointID)+"/download", nil)
	if err != nil {
		return checkpointDownloadView{}, err
	}
	m.attachHeaders(req)
	resp, err := m.client.Do(req)
	if err != nil {
		return checkpointDownloadView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return checkpointDownloadView{}, &syncErr{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var out checkpointDownloadView
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return checkpointDownloadView{}, fmt.Errorf("decode download urls: %w", err)
	}
	return out, nil
}

func (m *migrationClient) fetchBlob(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("blob GET %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// TODO(coderabbit): cap response body via io.LimitReader; surface typed err at limit
	// https://github.com/Acksell/clank/pull/16#discussion_r3213461997
	return io.ReadAll(resp.Body)
}

// applyToSprite POSTs a multipart checkpoint to the sprite's
// /sync/apply endpoint using the HostRef's transport (which injects
// the sprite-side bearer). worktreeID becomes the sprite-side
// directory name (~/work/<worktreeID>/), agreeing with
// host.Service.workDirFor's lookup convention.
//
// TODO(coderabbit): stream multipart via io.Pipe so bundle bytes don't all sit in RAM
// https://github.com/Acksell/clank/pull/16#discussion_r3213461995
func (m *migrationClient) applyToSprite(ctx context.Context, spriteURL string, transport http.RoundTripper, authToken, worktreeID string, manifestBytes, headBytes, incrBytes []byte) error {
	if worktreeID == "" {
		return fmt.Errorf("worktree id is required")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := writePart(mw, "manifest", "manifest.json", "application/json", manifestBytes); err != nil {
		return err
	}
	if err := writePart(mw, "head_commit", "headCommit.bundle", "application/octet-stream", headBytes); err != nil {
		return err
	}
	if err := writePart(mw, "incremental", "incremental.bundle", "application/octet-stream", incrBytes); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}

	target := strings.TrimRight(spriteURL, "/") + "/sync/apply?repo=" + url.QueryEscape(worktreeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	cli := m.client
	if transport != nil {
		cli = &http.Client{Transport: transport, Timeout: cli.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("sprite apply %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (m *migrationClient) transferOwnership(ctx context.Context, worktreeID, newKind, newID, expectedOwnerID string) (worktreeView, error) {
	body, err := json.Marshal(map[string]string{
		"to_kind":           newKind,
		"to_id":             newID,
		"expected_owner_id": expectedOwnerID,
	})
	if err != nil {
		return worktreeView{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.syncURL+"/v1/worktrees/"+url.PathEscape(worktreeID)+"/owner",
		bytes.NewReader(body))
	if err != nil {
		return worktreeView{}, err
	}
	m.attachHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return worktreeView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyOut, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return worktreeView{}, &syncErr{Status: resp.StatusCode, Body: strings.TrimSpace(string(bodyOut))}
	}
	var out worktreeView
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return worktreeView{}, fmt.Errorf("decode transfer response: %w", err)
	}
	return out, nil
}

func (m *migrationClient) attachHeaders(req *http.Request) {
	req.Header.Set(HeaderDeviceID, m.deviceID)
	// TODO(coderabbit): forward Authorization to clank-sync once it stops accepting PermissiveAuth
	// https://github.com/Acksell/clank/pull/16#discussion_r3213461996
}

// respond maps a sync-server error to an HTTP status surfaced to the
// caller. 409 (concurrent migration) is preserved so the laptop UI
// can suggest a retry.
func (m *migrationClient) respond(w http.ResponseWriter, op string, err error) {
	if se, ok := err.(*syncErr); ok {
		http.Error(w, op+": "+se.Body, se.Status)
		return
	}
	http.Error(w, op+": "+err.Error(), http.StatusBadGateway)
}

type syncErr struct {
	Status int
	Body   string
}

func (e *syncErr) Error() string { return fmt.Sprintf("sync %d: %s", e.Status, e.Body) }

func writePart(mw *multipart.Writer, name, filename, contentType string, data []byte) error {
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name=%q; filename=%q`, name, filename)}
	h["Content-Type"] = []string{contentType}
	pw, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create part %s: %w", name, err)
	}
	if _, err := pw.Write(data); err != nil {
		return fmt.Errorf("write part %s: %w", name, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
