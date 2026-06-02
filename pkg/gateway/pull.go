package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// pullResponse is the body returned by POST /v1/worktrees/{id}/pull.
// The CLI downloads these presigned GET URLs and applies the checkpoint
// locally (see the clank CLI's applyRemotePull).
//
// SessionManifestURL + SessionBlobURLs ride alongside the code URLs
// when the sprite had opencode sessions in the worktree. The laptop
// fetches them after the code apply and imports them with sessionsync.
type pullHeadBundle struct {
	TipSHA  string `json:"tip_sha"`
	BaseSHA string `json:"base_sha,omitempty"`
	GetURL  string `json:"get_url"`
}

type pullResponse struct {
	CheckpointID string `json:"checkpoint_id"`
	ManifestURL  string `json:"manifest_url"`
	// HeadBundles is the ordered (oldest→newest) head chain to fetch+apply.
	HeadBundles        []pullHeadBundle  `json:"head_bundles"`
	UncommittedURL     string            `json:"uncommitted_url"`
	SessionManifestURL string            `json:"session_manifest_url,omitempty"`
	SessionBlobURLs    map[string]string `json:"session_blob_urls,omitempty"`
}

// handlePullWorktree orchestrates a sprite-to-laptop checkpoint pull: gateway
// tells sprite to build bundles, mints presigned S3 PUT URLs, triggers upload,
// commits the checkpoint, then returns GET URLs. The sprite holds no creds and
// makes no outbound calls except to S3 via presigned URLs (pure-responder model).
func (g *Gateway) handlePullWorktree(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "pull not configured (Sync unset)", http.StatusServiceUnavailable)
		return
	}
	userID := auth.MustPrincipal(r.Context()).UserID
	worktreeID := r.PathValue("id")
	if worktreeID == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}
	// The laptop's current HEAD, so we return only the head-chain slice it
	// lacks (empty ⇒ fresh applier gets the full chain).
	haveHead := r.URL.Query().Get("have_head")

	wt, err := g.cfg.Sync.GetWorktree(r.Context(), userID, worktreeID)
	if err != nil {
		syncErrToHTTP(w, "read worktree", err)
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
		UncommittedCommit: build.UncommittedCommit,
		CreatedBy:         "sprite:" + hostRef.HostID,
	})
	if err != nil {
		syncErrToHTTP(w, "create checkpoint", err)
		return
	}

	// Step 3: sprite PUTs the bundles to S3 via the presigned URLs.
	if err := triggerSpriteUpload(r.Context(), cli, hostRef, build.BuildID, spriteUploadParams{
		CheckpointID:   ck.CheckpointID,
		ManifestPutURL: ck.ManifestPutURL,
		// Empty when the server already has this HEAD bundle (dedup) —
		// the sprite skips the head upload in that case.
		HeadCommitPutURL:  ck.HeadBundlePutURL,
		UncommittedPutURL: ck.UncommittedURL,
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
	if _, err := g.cfg.Sync.CommitCheckpoint(r.Context(), userID, ck.CheckpointID, ck.HeadBundleBase); err != nil {
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
	gets, err := g.cfg.Sync.DownloadCheckpointURLs(r.Context(), userID, ck.CheckpointID, haveHead)
	if err != nil {
		syncErrToHTTP(w, "download checkpoint URLs", err)
		return
	}
	heads := make([]pullHeadBundle, len(gets.HeadBundles))
	for i, hb := range gets.HeadBundles {
		heads[i] = pullHeadBundle{TipSHA: hb.TipSHA, BaseSHA: hb.BaseSHA, GetURL: hb.GetURL}
	}

	writeJSON(w, http.StatusOK, pullResponse{
		CheckpointID:       ck.CheckpointID,
		ManifestURL:        gets.ManifestGetURL,
		HeadBundles:        heads,
		UncommittedURL:     gets.UncommittedURL,
		SessionManifestURL: sessionManifestGetURL,
		SessionBlobURLs:    sessionBlobGetURLs,
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
	UncommittedCommit string `json:"uncommitted_commit"`
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
	UncommittedPutURL string `json:"uncommitted_put_url"`
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
	BuildID string                 `json:"build_id"`
	Entries []spriteSessionEntry   `json:"entries"`
	Skipped []spriteSkippedSession `json:"skipped"`
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
