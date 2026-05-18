package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// ErrSessionUnknown is returned by ownerForSession when the local
// clank-host has no row for the given session ID. The dispatcher
// maps this to a 404 — the caller is responsible for importing the
// session (via `clank pull --migrate`) before opening it.
var ErrSessionUnknown = errors.New("gateway: session not in local clank-host")

// ownerForSession decides whether a session ID's worktree is local-
// or remote-owned. Two lookups under the hood:
//
//  1. GET /sessions/{id} on the local clank-host (microseconds —
//     localhost HTTP) to fetch the session row's worktree_id.
//  2. ownerCache.Lookup(worktree_id) to consult the cached view of
//     remote ownership.
//
// Returns ErrSessionUnknown when the local clank-host has no row.
// Returns OwnerKindLocal as a graceful default when the worktree is
// unknown to the cache (cold cache + offline remote, or the session
// has no worktree_id at all).
func (g *Gateway) ownerForSession(ctx context.Context, userID, sessionID string) (clanksync.OwnerKind, error) {
	if g.ownerCache == nil {
		// No cache configured → everything routes local. Used when
		// the daemon isn't laptop-mode (no active remote).
		return clanksync.OwnerKindLocal, nil
	}

	info, err := g.fetchLocalSession(ctx, userID, sessionID)
	if errors.Is(err, ErrSessionUnknown) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("gateway: lookup local session %s: %w", sessionID, err)
	}
	if info.GitRef.WorktreeID == "" {
		// No worktree linkage → can only be a local session. (E.g.
		// pre-sync sessions, or some out-of-band creation.)
		return clanksync.OwnerKindLocal, nil
	}

	kind, ok, err := g.ownerCache.Lookup(ctx, info.GitRef.WorktreeID)
	if err != nil {
		// Cache failed and had no prior data. Treat as local — we
		// don't want a transient remote-gateway hiccup to break
		// local sessions entirely.
		g.log.Printf("gateway: owner cache lookup for worktree %s failed; defaulting to local: %v", info.GitRef.WorktreeID, err)
		return clanksync.OwnerKindLocal, nil
	}
	if !ok {
		// Worktree not known to remote → local.
		return clanksync.OwnerKindLocal, nil
	}
	return kind, nil
}

// fetchLocalSession reads /sessions/{id} from the local clank-host
// subprocess via the provisioner's HostRef. Returns ErrSessionUnknown
// on 404.
func (g *Gateway) fetchLocalSession(ctx context.Context, userID, sessionID string) (agent.SessionInfo, error) {
	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("ensure host: %w", err)
	}
	target := strings.TrimRight(ref.URL, "/") + "/sessions/" + url.PathEscape(sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return agent.SessionInfo{}, err
	}
	cli := &http.Client{Transport: ref.Transport, Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return agent.SessionInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return agent.SessionInfo{}, ErrSessionUnknown
	}
	if resp.StatusCode != http.StatusOK {
		return agent.SessionInfo{}, fmt.Errorf("local host /sessions/%s: HTTP %d", sessionID, resp.StatusCode)
	}
	var info agent.SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return agent.SessionInfo{}, fmt.Errorf("decode SessionInfo: %w", err)
	}
	return info, nil
}
