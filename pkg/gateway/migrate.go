package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// migrateRequest is the body of POST /v1/migrate/worktrees/{id}. Only
// to_remote uses this single-call shape — to_local goes through the
// two-phase materialize + commit endpoints because it must move data
// before flipping ownership.
type migrateRequest struct {
	Direction string `json:"direction"` // "to_remote" only
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
//  1. Read the worktree from sync; reject if not laptop-owned by
//     the calling device.
//  2. Pre-check that there's an uploaded checkpoint to migrate.
//  3. Wake the sprite via Provisioner.EnsureHost.
//  4. Download the latest checkpoint's bundles from object storage via
//     presigned GET URLs minted by the sync server.
//  5. Push the checkpoint to the sprite as a multipart /sync/apply
//     request — manifest + headCommit + incremental.
//  6. Atomic ownership transfer via the sync server. Loser of any race
//     gets 409 and surfaces it to the caller.
//
// Bundle bytes pass through the gateway's memory but are never
// persisted there. With encryption (P6), the gateway sees ciphertext
// only and cannot tamper because the sprite verifies the manifest
// signature.
func (g *Gateway) handleMigrateWorktree(w http.ResponseWriter, r *http.Request) {
	// Auth gates the deployment-state signal so an unauthenticated
	// caller can't probe whether migration is wired up.
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

	var req migrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Direction != "to_remote" {
		http.Error(w, "direction must be to_remote; use /v1/migrate/worktrees/{id}/materialize and /commit for to_local", http.StatusBadRequest)
		return
	}
	if !req.Confirm {
		http.Error(w, "confirm must be true", http.StatusBadRequest)
		return
	}

	wt, err := g.cfg.Sync.GetWorktree(r.Context(), userID, worktreeID)
	if err != nil {
		syncErrToHTTP(w, "read worktree", err)
		return
	}
	g.migrateToRemote(w, r, wt, userID)
}

// migrateToRemote is the §D happy path: pre-checked → wake → push →
// atomic transfer. Idempotent for re-runs against the same sprite —
// if the worktree is already owned by some sprite for this user, we
// no-op rather than 403, which means a user who calls migrate twice
// gets the same answer the second time.
func (g *Gateway) migrateToRemote(w http.ResponseWriter, r *http.Request, wt clanksync.Worktree, userID string) {
	if wt.OwnerKind == clanksync.OwnerKindRemote {
		// Already sprite-owned. Treat as no-op success.
		writeJSON(w, http.StatusOK, migrateResponse{
			WorktreeID:   wt.ID,
			NewOwnerKind: string(wt.OwnerKind),
			NewOwnerID:   wt.OwnerID,
			CheckpointID: wt.LatestSyncedCheckpoint,
		})
		return
	}
	if wt.OwnerKind != clanksync.OwnerKindLocal {
		http.Error(w, "worktree is not local-owned", http.StatusForbidden)
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

	cli := &http.Client{Timeout: 5 * time.Minute}

	// Pull-based: hand the sprite presigned GET URLs and let it fetch
	// the bundles directly from object storage. Bundle bytes never
	// traverse this process. On a 403/expired-URL response we re-mint
	// fresh URLs once and retry — anything else surfaces immediately.
	if err := g.pushCheckpointToSprite(r.Context(), cli, hostRef, wt, userID); err != nil {
		g.log.Printf("gateway migrate: apply to sprite: %v", err)
		http.Error(w, "apply to sprite: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Session leg — pushes opencode session blobs to the sprite so the
	// user can continue conversations after the worktree migrates. If
	// the checkpoint has no session manifest (pre-feature checkpoints
	// or empty worktrees), this is a no-op. Failures here block the
	// ownership transfer so the user can retry without ending up with
	// a half-migrated worktree (code applied, sessions missing).
	if err := g.pushSessionsToSprite(r.Context(), cli, hostRef, wt, userID); err != nil {
		g.log.Printf("gateway migrate: apply sessions to sprite: %v", err)
		http.Error(w, "apply sessions to sprite: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Empty expectedOwnerID — for local→remote we don't disambiguate
	// laptops anymore; ownership is per-user. TransferOwnership still
	// uses optimistic concurrency keyed on the (kind, id) tuple.
	updated, err := g.cfg.Sync.TransferOwnership(r.Context(), userID, wt.ID, clanksync.OwnerKindRemote, hostRef.HostID, "")
	if err != nil {
		syncErrToHTTP(w, "transfer ownership", err)
		return
	}

	writeJSON(w, http.StatusOK, migrateResponse{
		WorktreeID:   updated.ID,
		NewOwnerKind: string(updated.OwnerKind),
		NewOwnerID:   updated.OwnerID,
		CheckpointID: wt.LatestSyncedCheckpoint,
	})
}

// pushCheckpointToSprite mints presigned GET URLs for the worktree's
// latest synced checkpoint and POSTs them to the sprite's
// /sync/apply-from-urls. The sprite downloads the bundles itself and
// applies them; bundle bytes never traverse this process.
//
// On a url_expired response (S3 returned 403, typically because the
// 5-minute TTL elapsed during a slow sprite cold-start), the gateway
// mints fresh URLs and retries exactly once. Other error codes
// propagate without retry.
func (g *Gateway) pushCheckpointToSprite(ctx context.Context, cli *http.Client, hostRef provisioner.HostRef, wt clanksync.Worktree, userID string) error {
	for attempt := 0; attempt < 2; attempt++ {
		ck, err := g.cfg.Sync.DownloadCheckpointURLs(ctx, userID, wt.LatestSyncedCheckpoint)
		if err != nil {
			return fmt.Errorf("download checkpoint URLs: %w", err)
		}
		code, err := applyFromURLsToSprite(ctx, cli, hostRef.URL, hostRef.Transport, hostRef.AuthToken, wt.ID, ck.ManifestGetURL, ck.HeadCommitGetURL, ck.IncrementalURL)
		if err == nil {
			return nil
		}
		if code == "url_expired" && attempt == 0 {
			g.log.Printf("gateway migrate: sprite reported url_expired, retrying with fresh URLs")
			continue
		}
		return err
	}
	return fmt.Errorf("apply to sprite: exhausted retries")
}

// applyFromURLsToSprite POSTs JSON `{repo, manifest_url, head_commit_url,
// incremental_url}` to the sprite's /sync/apply-from-urls endpoint. The
// HostRef's transport injects the sprite-side bearer. Returns a typed
// error code (from internal/host/mux/sync.go) so callers can branch on
// "url_expired" for a one-shot retry.
func applyFromURLsToSprite(ctx context.Context, baseClient *http.Client, spriteURL string, transport http.RoundTripper, authToken, worktreeID, manifestURL, headURL, incrURL string) (errCode string, err error) {
	if worktreeID == "" {
		return "", fmt.Errorf("worktree id is required")
	}
	body, err := json.Marshal(map[string]string{
		"repo":             worktreeID,
		"manifest_url":     manifestURL,
		"head_commit_url":  headURL,
		"incremental_url":  incrURL,
	})
	if err != nil {
		return "", fmt.Errorf("marshal body: %w", err)
	}

	target := strings.TrimRight(spriteURL, "/") + "/sync/apply-from-urls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	cli := baseClient
	if transport != nil {
		cli = &http.Client{Transport: transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return "", nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var parsed struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	if parsed.Code != "" {
		return parsed.Code, fmt.Errorf("sprite apply-from-urls (%d, code=%s): %s", resp.StatusCode, parsed.Code, parsed.Error)
	}
	return "", fmt.Errorf("sprite apply-from-urls %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

// syncErrToHTTP maps errors from the sync.Server direct-call API to
// HTTP responses. ErrWorktreeNotFound → 404, ErrOwnerMismatch → 409,
// ErrForbidden → 403; everything else → 502.
func syncErrToHTTP(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, clanksync.ErrWorktreeNotFound):
		http.Error(w, op+": "+err.Error(), http.StatusNotFound)
	case errors.Is(err, clanksync.ErrOwnerMismatch):
		http.Error(w, op+": "+err.Error(), http.StatusConflict)
	case errors.Is(err, clanksync.ErrForbidden):
		http.Error(w, op+": "+err.Error(), http.StatusForbidden)
	default:
		http.Error(w, op+": "+err.Error(), http.StatusBadGateway)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
