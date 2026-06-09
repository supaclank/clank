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
	"github.com/acksell/clank/pkg/sync/checkpoint"
	"golang.org/x/sync/errgroup"
)

// Autosync (S3→sprite) is the reverse of handlePullWorktree and simpler:
// the laptop's checkpoint blobs already live in S3, so there's no
// build/upload/commit. The gateway mints presigned GET URLs and tells the
// sprite to apply them; the sprite enforces the fast-forward / clean /
// running-session guards (it owns the git state + session registry) and
// reports a typed outcome the gateway records on the worktree row.
//
// Trigger: the mobile homescreen calls POST /v1/worktrees/sync on open /
// pull-to-refresh (sync-all), plus POST /v1/worktrees/{id}/sync for the
// manual per-worktree button and conflict resolution (force=true).

// syncResult is the per-worktree outcome returned to the mobile client.
// State mirrors the sprite's applyResult states plus no_checkpoint (the
// laptop never pushed) and error (gateway/sprite failure).
type syncResult struct {
	WorktreeID   string `json:"worktree_id"`
	State        string `json:"state"`
	Detail       string `json:"detail,omitempty"`
	LocalHead    string `json:"local_head,omitempty"`
	IncomingHead string `json:"incoming_head,omitempty"`
}

// TODO(ai-review): extract applied/up_to_date/conflict/session_running into a shared package so the gateway can't silently drift from the sprite's wire values https://github.com/Acksell/clank/pull/40#discussion_r3342270613
const (
	syncStateApplied        = "applied"
	syncStateUpToDate       = "up_to_date"
	syncStateConflict       = "conflict"
	syncStateSessionRunning = "session_running"
	syncStateNoCheckpoint   = "no_checkpoint"
	syncStateError          = "error"
)

// defaultSyncFanoutLimit bounds how many worktrees sync-all materializes
// concurrently. Each worktree's sprite apply is independent and the sprite
// serializes per-repo, so a small pool collapses the per-worktree latency
// without flooding the sprite with simultaneous applies.
const defaultSyncFanoutLimit = 8

// syncAllResponse is the body of POST /v1/worktrees/sync.
type syncAllResponse struct {
	Results   []syncResult `json:"results"`
	Synced    int          `json:"synced"`
	Conflicts int          `json:"conflicts"`
}

// handleSyncAllWorktrees fast-forwards every compatible worktree onto the
// user's sprite. Triggered by the mobile homescreen (open + pull-to-
// refresh). One worktree's conflict/error never fails the batch — each
// gets its own result so the client can render the mix.
func (g *Gateway) handleSyncAllWorktrees(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "sync not configured (Sync unset)", http.StatusServiceUnavailable)
		return
	}
	userID := auth.MustPrincipal(r.Context()).UserID

	hostRef, err := g.cfg.Provisioner.EnsureHost(r.Context(), userID)
	if err != nil {
		g.log.Printf("gateway sync-all: EnsureHost(%s): %v", userID, err)
		http.Error(w, "ensure sprite: "+err.Error(), http.StatusBadGateway)
		return
	}
	wts, err := g.cfg.Sync.ListWorktrees(r.Context(), userID)
	if err != nil {
		http.Error(w, "list worktrees: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cli := &http.Client{Timeout: 5 * time.Minute}

	// Fan out across worktrees: each syncs independently and the sprite
	// serializes per-repo, so a bounded pool turns O(N × apply latency) into
	// roughly one apply latency. Results are written by index (no shared-slice
	// race); syncWorktreeToSprite encodes failures in State and never returns an
	// error, so the group never does either — every worktree is attempted.
	results := make([]syncResult, len(wts))
	eg := errgroup.Group{}
	eg.SetLimit(defaultSyncFanoutLimit)
	for i, wt := range wts {
		eg.Go(func() error {
			results[i] = g.syncWorktreeToSprite(r.Context(), cli, userID, hostRef, wt, false)
			return nil
		})
	}
	_ = eg.Wait()

	var synced, conflicts int
	for _, res := range results {
		switch res.State {
		case syncStateApplied, syncStateUpToDate:
			synced++
		case syncStateConflict:
			conflicts++
		}
	}
	writeJSON(w, http.StatusOK, syncAllResponse{Results: results, Synced: synced, Conflicts: conflicts})
}

// handleSyncWorktree syncs a single worktree onto the sprite. Serves both
// the manual per-worktree Sync button (force=false) and the conflict-
// resolution "discard sprite changes & pull laptop version" (force=true).
func (g *Gateway) handleSyncWorktree(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "sync not configured (Sync unset)", http.StatusServiceUnavailable)
		return
	}
	userID := auth.MustPrincipal(r.Context()).UserID
	worktreeID := r.PathValue("id")
	if worktreeID == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}
	// Body is optional ({} or absent ⇒ force=false).
	var body struct {
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	wt, err := g.cfg.Sync.GetWorktree(r.Context(), userID, worktreeID)
	if err != nil {
		syncErrToHTTP(w, "read worktree", err)
		return
	}
	hostRef, err := g.cfg.Provisioner.EnsureHost(r.Context(), userID)
	if err != nil {
		g.log.Printf("gateway sync: EnsureHost(%s): %v", userID, err)
		http.Error(w, "ensure sprite: "+err.Error(), http.StatusBadGateway)
		return
	}
	cli := &http.Client{Timeout: 5 * time.Minute}
	writeJSON(w, http.StatusOK, g.syncWorktreeToSprite(r.Context(), cli, userID, hostRef, wt, body.Force))
}

// syncWorktreeToSprite materializes/fast-forwards one worktree's latest
// pushed checkpoint (code + sessions) onto the sprite, then records the
// outcome on the worktree row (the display cache). It never returns an
// error — failures are encoded in the result's State so a sync-all batch
// keeps going.
func (g *Gateway) syncWorktreeToSprite(ctx context.Context, cli *http.Client, userID string, hostRef provisioner.HostRef, wt clanksync.Worktree, force bool) syncResult {
	res := syncResult{WorktreeID: wt.ID}

	if wt.LatestSyncedCheckpoint == "" {
		res.State = syncStateNoCheckpoint
		return res
	}
	ckID := wt.LatestSyncedCheckpoint

	// One metadata read serves both the early-exit session check and the
	// haveHead delta below. A missing checkpoint leaves ck zero/ckErr set; both
	// the gate and haveHead then conservatively no-op.
	ck, ckErr := g.cfg.Sync.GetCheckpoint(ctx, userID, ckID)

	// Early-exit: when the latest pushed checkpoint is already materialized on
	// this same host generation AND the sprite holds the same session set, the
	// sprite is provably current — return without dialing it or touching object
	// storage. Gated on the host id because MaterializedCheckpointID is a
	// display cache never cleared on reprovision, and a cold reprovision wipes
	// ~/work while minting a new id. The session-digest clause keeps a session-
	// only push (which bumps no checkpoint) from being wrongly skipped; an
	// empty/absent digest falls through to the authoritative path.
	if !force &&
		wt.MaterializedCheckpointID == ckID &&
		wt.MaterializedHostID == hostRef.HostID &&
		wt.SyncState == clanksync.SyncStateUpToDate &&
		ckErr == nil &&
		ck.SessionsContentDigest != "" &&
		ck.SessionsContentDigest == wt.SessionsSyncedHash {
		res.State = syncStateUpToDate
		return res
	}

	// haveHead lets DownloadCheckpointURLs ship only the head-bundle delta the
	// sprite is missing. Engage it only when the materialized checkpoint's HEAD
	// is a *different* commit than this checkpoint's HEAD — an equal HEAD yields
	// an empty chain the sprite's apply rejects (the session-only-push case,
	// where ckID == MaterializedCheckpointID). Gated on the host generation so
	// the sprite provably still holds that HEAD's objects; any uncertainty
	// falls back to "" (the full chain).
	haveHead := ""
	if ckErr == nil &&
		wt.MaterializedHostID == hostRef.HostID &&
		wt.MaterializedCheckpointID != "" &&
		wt.MaterializedCheckpointID != ckID {
		if mck, merr := g.cfg.Sync.GetCheckpoint(ctx, userID, wt.MaterializedCheckpointID); merr == nil && mck.HeadCommit != ck.HeadCommit {
			haveHead = mck.HeadCommit
		}
	}

	gets, err := g.cfg.Sync.DownloadCheckpointURLs(ctx, userID, ckID, haveHead)
	if err != nil {
		res.State = syncStateError
		res.Detail = "download checkpoint urls: " + err.Error()
		return res
	}
	headURLs := make([]string, len(gets.HeadBundles))
	for i, hb := range gets.HeadBundles {
		headURLs[i] = hb.GetURL
	}

	apply, err := triggerSpriteApply(ctx, cli, hostRef, spriteApplyParams{
		Repo:           wt.ID,
		ManifestURL:    gets.ManifestGetURL,
		HeadBundleURLs: headURLs,
		UncommittedURL: gets.UncommittedURL,
		Force:          force,
	})
	if err != nil {
		res.State = syncStateError
		res.Detail = "sprite apply: " + err.Error()
		return res
	}
	res.State = apply.State
	res.LocalHead = apply.LocalHead
	res.IncomingHead = apply.IncomingHead

	// Map the sprite outcome onto the worktree row. Preserve the existing
	// materialized pointer, session digest, and host generation on
	// conflict/busy (the sprite still holds whatever it had); advance them only
	// on a real apply.
	upd := clanksync.MaterializationUpdate{
		MaterializedCheckpointID: wt.MaterializedCheckpointID,
		SessionsSyncedHash:       wt.SessionsSyncedHash,
		MaterializedHostID:       wt.MaterializedHostID,
	}
	switch apply.State {
	case syncStateApplied, syncStateUpToDate:
		upd.MaterializedCheckpointID = ckID
		upd.MaterializedHostID = hostRef.HostID
		upd.SyncState = clanksync.SyncStateUpToDate
	case syncStateConflict:
		upd.SyncState = clanksync.SyncStateConflict
		upd.ConflictLocalHead = apply.LocalHead
		upd.ConflictRemoteHead = apply.IncomingHead
	case syncStateSessionRunning:
		upd.SyncState = clanksync.SyncStateBusy
	default:
		upd.SyncState = clanksync.SyncStateBehind
	}

	// Session leg: import the laptop's pushed sessions onto the (now
	// materialized) worktree. Session blobs upload straight to object storage
	// with no checkpoint bump or commit callback, so a session-only push
	// leaves apply.State up_to_date — the gateway can't be told sessions
	// changed. applySpriteSessions instead compares the manifest's content-
	// digest against what the sprite last imported (wt.SessionsSyncedHash) and
	// imports only on a change, so an unchanged refresh stays cheap. A fresh
	// code materialize (applied) forces a re-import: the sprite's $HOME volume
	// (host.db and ~/work alike) may have been reset.
	if apply.State == syncStateApplied || apply.State == syncStateUpToDate {
		force := apply.State == syncStateApplied
		hash, serr := g.applySpriteSessions(ctx, cli, hostRef, userID, wt.ID, ckID, wt.SessionsSyncedHash, force)
		if serr != nil {
			// Leave the row "behind" so the next sync retries (the digest
			// still won't match), and surface it without failing the code sync.
			upd.SyncState = clanksync.SyncStateBehind
			res.Detail = "session import failed: " + serr.Error()
		} else {
			upd.SessionsSyncedHash = hash
		}
	}

	if err := g.cfg.Sync.SetWorktreeMaterialization(ctx, userID, wt.ID, upd); err != nil {
		g.log.Printf("gateway sync: record materialization for %s: %v", wt.ID, err)
	}
	return res
}

// applySpriteSessions enumerates the checkpoint's sessions (by fetching +
// parsing the session manifest from object storage — the gateway has no
// sessions table) and tells the sprite to import them. It returns the
// manifest's content-digest so the caller can record what the sprite now
// holds (Worktree.SessionsSyncedHash).
//
// When the digest already matches prevHash the sprite holds this exact
// session set, so the import is skipped and prevHash is returned unchanged —
// unless force is set (a fresh code materialize, where the sprite may have
// been reset). A missing manifest (code-only checkpoint) is a no-op that
// returns the empty digest, not an error.
func (g *Gateway) applySpriteSessions(ctx context.Context, cli *http.Client, hostRef provisioner.HostRef, userID, worktreeID, checkpointID, prevHash string, force bool) (string, error) {
	// Fast path: the checkpoint row carries the manifest's content-digest
	// (persisted at presign time). When it already matches what the sprite
	// imported (prevHash), the sprite holds this exact session set — return
	// without the S3 manifest fetch. A fresh code materialize (force) re-
	// imports regardless; an empty digest (pre-v30 row, code-only push, or a
	// client that didn't send it) falls through to the authoritative fetch.
	if !force {
		if ck, err := g.cfg.Sync.GetCheckpoint(ctx, userID, checkpointID); err == nil &&
			ck.SessionsContentDigest != "" && ck.SessionsContentDigest == prevHash {
			return prevHash, nil
		}
	}

	// First hop: mint just the session-manifest URL (empty id slice).
	first, err := g.cfg.Sync.DownloadSessionURLs(ctx, userID, checkpointID, nil)
	if err != nil {
		return "", fmt.Errorf("mint session manifest url: %w", err)
	}
	manifestBytes, status, err := gatewayHTTPGet(ctx, cli, first.SessionManifestGetURL)
	if err != nil {
		// No session manifest blob ⇒ this checkpoint pushed no sessions.
		if status == http.StatusNotFound {
			return "", nil
		}
		return "", fmt.Errorf("fetch session manifest: %w", err)
	}
	manifest, err := checkpoint.UnmarshalSessionManifest(manifestBytes)
	if err != nil {
		// A malformed or pre-v2 manifest (left over from before the schema
		// bump) can never import — don't wedge the worktree as "behind"
		// forever. Log and skip; the next push regenerates a current manifest.
		g.log.Printf("session import: skipping worktree %s checkpoint %s: unreadable manifest: %v", worktreeID, checkpointID, err)
		return "", nil
	}
	digest := manifest.ContentDigest()
	// Sprite already holds this exact set — skip the round-trip unless a
	// fresh materialize forces a re-import.
	if !force && digest == prevHash {
		return digest, nil
	}
	if len(manifest.Sessions) == 0 {
		return digest, nil
	}
	refs := make([]checkpoint.SessionBlobRef, len(manifest.Sessions))
	for i, s := range manifest.Sessions {
		refs[i] = s.BlobRef()
	}
	full, err := g.cfg.Sync.DownloadSessionURLs(ctx, userID, checkpointID, refs)
	if err != nil {
		return "", fmt.Errorf("mint session blob urls: %w", err)
	}
	if err := triggerSpriteSessionApply(ctx, cli, hostRef, worktreeID, full.SessionManifestGetURL, full.SessionGetURLs); err != nil {
		return "", err
	}
	return digest, nil
}

// --- sprite RPC helpers --------------------------------------------
//
// Same shape as pull.go's triggerSprite* helpers (see the TODO there
// about collapsing them into one doSpriteRequest).

// spriteApplyResult mirrors hostmux.applyResult (the 200 body of
// /sync/apply-from-urls).
type spriteApplyResult struct {
	State        string `json:"state"`
	LocalHead    string `json:"local_head"`
	IncomingHead string `json:"incoming_head"`
}

// spriteApplyParams mirrors hostmux.applyFromURLsRequest.
type spriteApplyParams struct {
	Repo           string   `json:"repo"`
	ManifestURL    string   `json:"manifest_url"`
	HeadBundleURLs []string `json:"head_bundle_urls"`
	UncommittedURL string   `json:"uncommitted_url"`
	Force          bool     `json:"force"`
}

// triggerSpriteApply POSTs to /sync/apply-from-urls and decodes the typed
// outcome. A non-200 is a genuine failure (bad manifest, S3 unreachable,
// apply error); the four non-error outcomes ride in the 200 body.
func triggerSpriteApply(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, params spriteApplyParams) (spriteApplyResult, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return spriteApplyResult{}, fmt.Errorf("marshal: %w", err)
	}
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/apply-from-urls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return spriteApplyResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := spriteClient(baseClient, hostRef)
	resp, err := cli.Do(req)
	if err != nil {
		return spriteApplyResult{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return spriteApplyResult{}, fmt.Errorf("sprite apply %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out spriteApplyResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return spriteApplyResult{}, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// triggerSpriteSessionApply POSTs to /sync/sessions/apply-from-urls. The
// sprite fetches each session blob from S3 and imports it. Returns nil on
// the sprite's 204.
func triggerSpriteSessionApply(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, worktreeID, manifestURL string, blobURLs map[string]string) error {
	body, err := json.Marshal(map[string]any{
		"worktree_id":          worktreeID,
		"session_manifest_url": manifestURL,
		"session_blob_urls":    blobURLs,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	target := strings.TrimRight(hostRef.URL, "/") + "/sync/sessions/apply-from-urls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if hostRef.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostRef.AuthToken)
	}
	cli := spriteClient(baseClient, hostRef)
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("sprite sessions/apply %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
}

// spriteClient returns the HTTP client to use for a sprite request,
// swapping in the provisioner-supplied transport when present (same
// pattern as pull.go's helpers).
func spriteClient(base *http.Client, hostRef provisioner.HostRef) *http.Client {
	if hostRef.Transport != nil {
		return &http.Client{Transport: hostRef.Transport, Timeout: base.Timeout}
	}
	return base
}

// gatewayHTTPGet fetches an S3 presigned URL, returning the body and the
// HTTP status (so callers can distinguish a missing object from a real
// error).
func gatewayHTTPGet(ctx context.Context, cli *http.Client, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("GET %d", resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}
