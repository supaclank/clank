package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner"
)

// errSpriteWorktreeBusy marks a sprite-side delete refused because a session
// is actively running on the worktree (the host returned 409). The handler
// maps it back to 409 so the client sees "busy" — distinct from a
// transport/5xx failure that warrants a plain retry.
var errSpriteWorktreeBusy = errors.New("gateway: worktree has an active session on the sprite")

// handleDeleteWorktree services DELETE /v1/worktrees/{id} — full-cleanup
// worktree delete. Strict and ordered: the sprite's materialized state (its
// ~/work/<id> directory and the worktree's sessions) is removed FIRST; only
// if that succeeds is the sync row (and its checkpoint rows) deleted. A
// sprite failure aborts with 502 and leaves the record intact, so the client
// can retry once the sprite is reachable — host cleanup is idempotent. A
// never-materialized worktree has nothing to clean and succeeds trivially.
// Shared, content-addressed checkpoint blob bytes are reference-GC'd, not
// deleted here.
func (g *Gateway) handleDeleteWorktree(w http.ResponseWriter, r *http.Request) {
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

	// Tenancy: 404 if missing/already-deleted, 403 if owned by another user.
	if _, err := g.cfg.Sync.GetWorktree(r.Context(), userID, worktreeID); err != nil {
		syncErrToHTTP(w, "read worktree", err)
		return
	}

	// Wake the sprite and strip its materialized copy + sessions FIRST, so a
	// failure here leaves the record fully intact for a retry.
	hostRef, err := g.cfg.Provisioner.EnsureHost(r.Context(), userID)
	if err != nil {
		g.log.Printf("gateway delete: EnsureHost(%s): %v", userID, err)
		http.Error(w, "ensure sprite: "+err.Error(), http.StatusBadGateway)
		return
	}
	cli := &http.Client{Timeout: 5 * time.Minute}
	if err := triggerSpriteWorktreeRemove(r.Context(), cli, hostRef, worktreeID); err != nil {
		if errors.Is(err, errSpriteWorktreeBusy) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		g.log.Printf("gateway delete: sprite remove worktree %s: %v", worktreeID, err)
		http.Error(w, "sprite cleanup failed (worktree left intact — retry): "+err.Error(), http.StatusBadGateway)
		return
	}

	// Sprite is clean — now drop the sync row + its checkpoint rows.
	if err := g.cfg.Sync.DeleteWorktree(r.Context(), userID, worktreeID); err != nil {
		syncErrToHTTP(w, "delete worktree", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// triggerSpriteWorktreeRemove DELETEs /worktrees/{id} on the sprite,
// removing its materialized ~/work/<id> directory and the worktree's
// sessions. Mirrors the triggerSprite* helpers in sync.go (shared
// spriteClient + bearer header). Success on 204; a 409 maps to
// errSpriteWorktreeBusy so the caller can surface "busy" distinctly.
func triggerSpriteWorktreeRemove(ctx context.Context, baseClient *http.Client, hostRef provisioner.HostRef, worktreeID string) error {
	target := strings.TrimRight(hostRef.URL, "/") + "/worktrees/" + url.PathEscape(worktreeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
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
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %s", errSpriteWorktreeBusy, strings.TrimSpace(string(preview)))
	}
	return fmt.Errorf("sprite delete worktree %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
}
