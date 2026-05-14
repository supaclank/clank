package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// migrationTokenTTL bounds how long a materialize → commit pair can
// straddle. 10 minutes is generous enough for a slow apply on a big
// worktree and short enough that a stale token doesn't grant
// indefinite commit authority.
const migrationTokenTTL = 10 * time.Minute

// materializeResponse is the body returned by
// POST /v1/migrate/worktrees/{id}/materialize. The CLI feeds these
// fields back into the commit call after downloading + applying the
// checkpoint locally.
//
// SessionManifestURL + SessionBlobURLs ride alongside the code URLs
// when the sprite had opencode sessions in the worktree. The laptop
// fetches them after the code apply and hands them to its local
// clank-host's /sync/sessions/apply-from-urls.
type materializeResponse struct {
	CheckpointID         string            `json:"checkpoint_id"`
	HeadCommit           string            `json:"head_commit"`
	ManifestURL          string            `json:"manifest_url"`
	HeadCommitURL        string            `json:"head_commit_url"`
	IncrementalURL       string            `json:"incremental_url"`
	SessionManifestURL   string            `json:"session_manifest_url,omitempty"`
	SessionBlobURLs      map[string]string `json:"session_blob_urls,omitempty"`
	MigrationToken       string            `json:"migration_token"`
	MigrationExpiry      int64             `json:"migration_expiry"` // unix seconds
}

// commitRequest is the body for /commit. The migration token gates
// this call: it proves the laptop just materialized the named
// checkpoint and is calling commit on the same migration attempt.
type commitRequest struct {
	CheckpointID   string `json:"checkpoint_id"`
	MigrationToken string `json:"migration_token"`
}

// handleMigrateMaterialize orchestrates a sprite-to-laptop checkpoint
// pull. Sprite-as-pure-responder model: gateway tells the sprite to
// build bundles, gateway mints presigned PUT URLs from its in-process
// sync server, gateway tells the sprite to upload to S3 via those URLs,
// gateway commits the checkpoint. Sprite holds no credentials and makes
// no outbound HTTP calls except to S3 via short-lived presigned URLs.
//
// No ownership change yet — the matching /commit call flips ownership
// after the laptop has successfully downloaded + applied the bundles.
func (g *Gateway) handleMigrateMaterialize(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "migration not configured (Sync unset)", http.StatusServiceUnavailable)
		return
	}
	userID := auth.MustPrincipal(r.Context()).UserID
	worktreeID := r.PathValue("id")
	if worktreeID == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}

	wt, err := g.cfg.Sync.GetWorktree(r.Context(), userID, worktreeID)
	if err != nil {
		syncErrToHTTP(w, "read worktree", err)
		return
	}
	if wt.OwnerKind != clanksync.OwnerKindRemote {
		http.Error(w, "worktree is not currently sprite-owned (nothing to materialize)", http.StatusConflict)
		return
	}

	hostRef, err := g.cfg.Provisioner.EnsureHost(r.Context(), userID)
	if err != nil {
		g.log.Printf("gateway materialize: EnsureHost(%s): %v", userID, err)
		http.Error(w, "ensure sprite: "+err.Error(), http.StatusBadGateway)
		return
	}

	cli := &http.Client{Timeout: 5 * time.Minute}

	// Step 1: sprite builds bundles to local disk, returns metadata.
	build, err := triggerSpriteBuild(r.Context(), cli, hostRef, wt.ID)
	if err != nil {
		g.log.Printf("gateway materialize: sprite build: %v", err)
		http.Error(w, "sprite build: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Idempotent cleanup. The sprite's upload handler deletes the
	// build on success, so this DELETE is a no-op when the happy path
	// completes; on failure (gateway exits between steps) it reclaims
	// the sprite's local disk eagerly without waiting for the reaper.
	defer func() {
		_ = deleteSpriteBuild(context.Background(), cli, hostRef, build.BuildID)
	}()

	// Step 2: gateway creates the checkpoint row + mints presigned PUT URLs.
	ck, err := g.cfg.Sync.CreateCheckpoint(r.Context(), userID, clanksync.CreateCheckpointRequest{
		WorktreeID:        wt.ID,
		HeadCommit:        build.HeadCommit,
		HeadRef:           build.HeadRef,
		IndexTree:         build.IndexTree,
		WorktreeTree:      build.WorktreeTree,
		IncrementalCommit: build.IncrementalCommit,
		CreatedBy:         "sprite:" + hostRef.HostID,
	})
	if err != nil {
		syncErrToHTTP(w, "create checkpoint", err)
		return
	}

	// Step 3: sprite PUTs the bundles to S3 via the presigned URLs.
	if err := triggerSpriteUpload(r.Context(), cli, hostRef, build.BuildID, spriteUploadParams{
		CheckpointID:      ck.CheckpointID,
		ManifestPutURL:    ck.ManifestPutURL,
		HeadCommitPutURL:  ck.HeadCommitPutURL,
		IncrementalPutURL: ck.IncrementalURL,
	}); err != nil {
		g.log.Printf("gateway materialize: sprite upload: %v", err)
		http.Error(w, "sprite upload: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Step 4: session leg. Mirrors the code-leg three-step: sprite
	// builds session blobs → gateway mints presigned PUT URLs →
	// sprite uploads. Skipped silently when the sprite has no
	// opencode sessions in this worktree (sessionBuild.Entries is
	// empty). Critically, this runs BEFORE CommitCheckpoint so a
	// session-leg failure can't leave latest_synced_checkpoint
	// pointing at a checkpoint whose session blobs are missing from
	// storage.
	var sessionIDs []string
	sessionBuild, err := triggerSpriteSessionBuild(r.Context(), cli, hostRef, wt.ID, ck.CheckpointID)
	if err != nil {
		g.log.Printf("gateway materialize: sprite session build: %v", err)
		http.Error(w, "sprite session build: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() {
		_ = deleteSpriteSessionBuild(context.Background(), cli, hostRef, sessionBuild.BuildID)
	}()
	if len(sessionBuild.Entries) > 0 {
		sessionIDs = make([]string, len(sessionBuild.Entries))
		for i, e := range sessionBuild.Entries {
			sessionIDs[i] = e.SessionID
		}
		presign, err := g.cfg.Sync.PresignSessionPuts(r.Context(), userID, clanksync.SessionPresignRequest{
			CheckpointID: ck.CheckpointID,
			SessionIDs:   sessionIDs,
		})
		if err != nil {
			syncErrToHTTP(w, "presign session puts", err)
			return
		}
		if err := triggerSpriteSessionUpload(r.Context(), cli, hostRef, sessionBuild.BuildID, spriteSessionUploadParams{
			CheckpointID:          ck.CheckpointID,
			SessionURLs:           presign.SessionPutURLs,
			SessionManifestPutURL: presign.SessionManifestPutURL,
		}); err != nil {
			g.log.Printf("gateway materialize: sprite session upload: %v", err)
			http.Error(w, "sprite session upload: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	// Step 5: gateway commits the checkpoint (advances
	// latest_synced_checkpoint after verifying the code blobs are in
	// storage). Runs only after both code AND session uploads
	// succeeded so a partial-upload failure can't leak a pointer
	// advance.
	if _, err := g.cfg.Sync.CommitCheckpoint(r.Context(), userID, ck.CheckpointID); err != nil {
		syncErrToHTTP(w, "commit checkpoint", err)
		return
	}

	// Step 6: mint presigned GET URLs (both code and session) for
	// the laptop. DownloadSessionURLs requires the checkpoint to be
	// committed (UploadedAt set), so it has to run after step 5.
	var sessionManifestGetURL string
	var sessionBlobGetURLs map[string]string
	if len(sessionIDs) > 0 {
		sessionGets, err := g.cfg.Sync.DownloadSessionURLs(r.Context(), userID, ck.CheckpointID, sessionIDs)
		if err != nil {
			syncErrToHTTP(w, "download session URLs", err)
			return
		}
		sessionManifestGetURL = sessionGets.SessionManifestGetURL
		sessionBlobGetURLs = sessionGets.SessionGetURLs
	}
	gets, err := g.cfg.Sync.DownloadCheckpointURLs(r.Context(), userID, ck.CheckpointID)
	if err != nil {
		syncErrToHTTP(w, "download checkpoint URLs", err)
		return
	}

	expiry := time.Now().Add(migrationTokenTTL).Unix()
	token := g.signMigrationToken(wt.ID, ck.CheckpointID, userID, expiry)

	writeJSON(w, http.StatusOK, materializeResponse{
		CheckpointID:       ck.CheckpointID,
		HeadCommit:         build.HeadCommit,
		ManifestURL:        gets.ManifestGetURL,
		HeadCommitURL:      gets.HeadCommitGetURL,
		IncrementalURL:     gets.IncrementalURL,
		SessionManifestURL: sessionManifestGetURL,
		SessionBlobURLs:    sessionBlobGetURLs,
		MigrationToken:     token,
		MigrationExpiry:    expiry,
	})
}

// handleMigrateCommit verifies the migration token, double-checks that
// the sync server's latest_synced_checkpoint still points at the one
// the laptop just applied, and atomically transfers ownership.
func (g *Gateway) handleMigrateCommit(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "migration not configured (Sync unset)", http.StatusServiceUnavailable)
		return
	}
	userID := auth.MustPrincipal(r.Context()).UserID
	worktreeID := r.PathValue("id")
	if worktreeID == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}

	var req commitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.CheckpointID == "" || req.MigrationToken == "" {
		http.Error(w, "checkpoint_id and migration_token are required", http.StatusBadRequest)
		return
	}
	if !g.verifyMigrationToken(req.MigrationToken, worktreeID, req.CheckpointID, userID) {
		http.Error(w, "invalid or expired migration_token", http.StatusForbidden)
		return
	}

	wt, err := g.cfg.Sync.GetWorktree(r.Context(), userID, worktreeID)
	if err != nil {
		syncErrToHTTP(w, "read worktree", err)
		return
	}
	if wt.LatestSyncedCheckpoint != req.CheckpointID {
		http.Error(w, "newer checkpoint exists; re-run materialize", http.StatusConflict)
		return
	}
	if wt.OwnerKind != clanksync.OwnerKindRemote {
		http.Error(w, "worktree is no longer sprite-owned", http.StatusConflict)
		return
	}

	// New local owner ID is empty — ownership is per-user, not
	// per-device; OwnerID is only meaningful for remote (sprite) owners.
	updated, err := g.cfg.Sync.TransferOwnership(r.Context(), userID, wt.ID, clanksync.OwnerKindLocal, "", wt.OwnerID)
	if err != nil {
		syncErrToHTTP(w, "transfer ownership", err)
		return
	}

	writeJSON(w, http.StatusOK, migrateResponse{
		WorktreeID:   updated.ID,
		NewOwnerKind: string(updated.OwnerKind),
		NewOwnerID:   updated.OwnerID,
		CheckpointID: req.CheckpointID,
	})
}

// --- sprite RPC helpers --------------------------------------------

// spriteBuildResult mirrors the JSON body of POST /sync/build's response
// (internal/host/mux/sync.go's buildResponse).
type spriteBuildResult struct {
	BuildID           string `json:"build_id"`
	HeadCommit        string `json:"head_commit"`
	HeadRef           string `json:"head_ref"`
	IndexTree         string `json:"index_tree"`
	WorktreeTree      string `json:"worktree_tree"`
	IncrementalCommit string `json:"incremental_commit"`
}

// TODO(coderabbit): collapse the six sprite-request helpers below
// (triggerSpriteBuild/Upload/delete + their session-leg twins) into a
// single doSpriteRequest(ctx, hostRef, method, path, body) helper.
// Six near-identical NewRequestWithContext+Authorization+Transport
// blocks ought to share one. https://github.com/Acksell/clank/pull/18
//
// triggerSpriteBuild POSTs to /sync/build?repo=<id> on the sprite.
func triggerSpriteBuild(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, worktreeID string) (*spriteBuildResult, error) {
	if worktreeID == "" {
		return nil, fmt.Errorf("worktree id is required")
	}
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/build?repo=" + worktreeID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return nil, err
	}
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := baseClient
	if hostRef.Transport != nil {
		cli = &http.Client{Transport: hostRef.Transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sprite build %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out spriteBuildResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if out.BuildID == "" {
		return nil, fmt.Errorf("sprite returned empty build_id")
	}
	return &out, nil
}

// spriteUploadParams is the JSON body of POST /sync/builds/{id}/upload.
type spriteUploadParams struct {
	CheckpointID      string `json:"checkpoint_id"`
	ManifestPutURL    string `json:"manifest_put_url"`
	HeadCommitPutURL  string `json:"head_commit_put_url"`
	IncrementalPutURL string `json:"incremental_put_url"`
}

// triggerSpriteUpload POSTs to /sync/builds/{id}/upload on the sprite.
// Sprite PUTs the bundles to S3 using the supplied presigned URLs.
func triggerSpriteUpload(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, buildID string, params spriteUploadParams) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/builds/" + buildID + "/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := baseClient
	if hostRef.Transport != nil {
		cli = &http.Client{Transport: hostRef.Transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("sprite upload %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
}

// deleteSpriteBuild DELETEs a build on the sprite. Best-effort
// cleanup; the sprite's reaper picks up orphans we miss.
func deleteSpriteBuild(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, buildID string) error {
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/builds/" + buildID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := baseClient
	if hostRef.Transport != nil {
		cli = &http.Client{Transport: hostRef.Transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// --- session-leg sprite RPC helpers --------------------------------
//
// Mirror the code-leg helpers above for the session export blobs. The
// sprite's handlers live in internal/host/mux/sessions_sync.go; the
// gateway here orchestrates them the same way it orchestrates the
// code build/upload pair.

// spriteSessionBuildResult mirrors the JSON body of POST
// /sync/sessions/build's response (sessionBuildResponse in
// internal/host/mux/sessions_sync.go).
type spriteSessionBuildResult struct {
	BuildID string                                   `json:"build_id"`
	Entries []spriteSessionEntry                     `json:"entries"`
	Skipped []spriteSkippedSession                   `json:"skipped"`
}

// spriteSessionEntry is the on-the-wire shape of
// checkpoint.SessionEntry. We mirror it locally (rather than
// importing the checkpoint package) to keep the gateway's wire-
// format dependencies minimal — only SessionID is needed by the
// gateway, the rest passes through opaquely.
type spriteSessionEntry struct {
	SessionID string `json:"session_id"`
	// other fields exist on the wire but the gateway doesn't read them
}

// spriteSkippedSession mirrors host.SkippedSession; surfaced so the
// CLI can warn the user about non-opencode sessions that were
// excluded.
type spriteSkippedSession struct {
	SessionID string `json:"session_id"`
	Backend   string `json:"backend"`
	Reason    string `json:"reason"`
}

// triggerSpriteSessionBuild POSTs to /sync/sessions/build on the sprite.
// The sprite quiesces + exports every session in the worktree to local
// temp files and returns the manifest entries + a build_id.
func triggerSpriteSessionBuild(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, worktreeID, checkpointID string) (*spriteSessionBuildResult, error) {
	if worktreeID == "" || checkpointID == "" {
		return nil, fmt.Errorf("worktree_id and checkpoint_id are required")
	}
	body, err := json.Marshal(map[string]string{
		"worktree_id":   worktreeID,
		"checkpoint_id": checkpointID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/sessions/build"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := baseClient
	if hostRef.Transport != nil {
		cli = &http.Client{Transport: hostRef.Transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sprite sessions/build %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out spriteSessionBuildResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if out.BuildID == "" {
		return nil, fmt.Errorf("sprite returned empty build_id for session build")
	}
	return &out, nil
}

// spriteSessionUploadParams is the JSON body of POST
// /sync/sessions/builds/{id}/upload.
type spriteSessionUploadParams struct {
	CheckpointID          string            `json:"checkpoint_id"`
	SessionURLs           map[string]string `json:"session_urls"`
	SessionManifestPutURL string            `json:"session_manifest_put_url"`
}

// triggerSpriteSessionUpload POSTs to /sync/sessions/builds/{id}/upload
// on the sprite. The sprite PUTs each session blob to S3 via the
// presigned URLs in the body. Returns nil on the sprite's 204; any
// other status is wrapped with the response body for diagnostics.
func triggerSpriteSessionUpload(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, buildID string, params spriteSessionUploadParams) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/sessions/builds/" + buildID + "/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := baseClient
	if hostRef.Transport != nil {
		cli = &http.Client{Transport: hostRef.Transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("sprite sessions/upload %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
}

// deleteSpriteSessionBuild DELETEs a session build on the sprite.
// Best-effort cleanup; sprite's reaper handles orphans.
func deleteSpriteSessionBuild(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, buildID string) error {
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/sessions/builds/" + buildID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := baseClient
	if hostRef.Transport != nil {
		cli = &http.Client{Transport: hostRef.Transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// --- migration token -----------------------------------------------

// signMigrationToken issues an HMAC-SHA256 over
// "<worktreeID>:<checkpointID>:<userID>:<expiryUnix>" using the
// gateway's migrationKey. The expiry is encoded in the token itself
// so verification doesn't need extra state. Binding to userID
// prevents one user's token from being replayed by another.
func (g *Gateway) signMigrationToken(worktreeID, checkpointID, userID string, expiry int64) string {
	payload := fmt.Sprintf("%s:%s:%s:%d", worktreeID, checkpointID, userID, expiry)
	mac := hmac.New(sha256.New, g.migrationKey)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return strconv.FormatInt(expiry, 10) + "." + sig
}

// verifyMigrationToken returns true iff sig matches the recomputed HMAC
// for the given fields and the embedded expiry is in the future.
func (g *Gateway) verifyMigrationToken(token, worktreeID, checkpointID, userID string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > expiry {
		return false
	}
	payload := fmt.Sprintf("%s:%s:%s:%d", worktreeID, checkpointID, userID, expiry)
	mac := hmac.New(sha256.New, g.migrationKey)
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(parts[1]))
}
