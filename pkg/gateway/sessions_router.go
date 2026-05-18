package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/auth"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// handleListSessions services GET /sessions: fetches the laptop's
// local clank-host sessions, then decorates each row with `is_remote`
// derived from the OwnerCache. No fan-out to the remote — the cost
// model (sprite-wake on /sessions proxy-through) makes that
// unaffordable on every TUI inbox refresh.
func (g *Gateway) handleListSessions(w http.ResponseWriter, r *http.Request) {
	g.serveDecoratedSessionList(w, r, "/sessions")
}

// handleSearchSessions services GET /sessions/search. Identical to
// the list endpoint plus query-string passthrough.
func (g *Gateway) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	path := "/sessions/search"
	if raw := r.URL.RawQuery; raw != "" {
		path += "?" + raw
	}
	g.serveDecoratedSessionList(w, r, path)
}

// serveDecoratedSessionList fetches the upstream JSON list from the
// local clank-host and decorates each element with an `is_remote`
// boolean. Both /sessions and /sessions/search return the same shape
// (a top-level JSON array of SessionInfo); we tolerate either an
// array or `{"sessions":[...]}` for future-proofing.
func (g *Gateway) serveDecoratedSessionList(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	userID := auth.MustPrincipal(r.Context()).UserID

	body, contentType, err := g.fetchLocalUpstream(r.Context(), userID, r.Method, upstreamPath)
	if err != nil {
		if errors.Is(err, ErrSessionUnknown) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		g.log.Printf("gateway list-sessions: %v", err)
		http.Error(w, "list sessions: "+err.Error(), http.StatusBadGateway)
		return
	}

	decorated, err := decorateSessionsBody(r.Context(), body, g.ownerCache)
	if err != nil {
		g.log.Printf("gateway list-sessions decorate: %v", err)
		// Fall back to forwarding the raw body — the cloud icon won't
		// render but listing still works. Better than a 5xx because
		// the data is fine.
		decorated = body
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(decorated)
}

// decorateSessionsBody parses the upstream's session-list response,
// adds an `is_remote` field to each entry, and re-encodes. Tolerates
// both `[...]` and `{"sessions":[...]}` shapes. On any decode failure
// returns the raw body unchanged.
func decorateSessionsBody(ctx context.Context, body []byte, cache *OwnerCache) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body, nil
	}

	if trimmed[0] == '[' {
		var rows []map[string]any
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("decode sessions array: %w", err)
		}
		decorateRows(ctx, rows, cache)
		return json.Marshal(rows)
	}

	var envelope map[string]any
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, fmt.Errorf("decode sessions envelope: %w", err)
	}
	if rows, ok := envelope["sessions"].([]any); ok {
		mapped := make([]map[string]any, 0, len(rows))
		for _, v := range rows {
			if m, ok := v.(map[string]any); ok {
				mapped = append(mapped, m)
			}
		}
		decorateRows(ctx, mapped, cache)
		envelope["sessions"] = mapped
	}
	return json.Marshal(envelope)
}

// decorateRows annotates each session row with `is_remote` based on
// its git_ref.worktree_id. Sessions with no worktree linkage default
// to `is_remote=false`.
func decorateRows(ctx context.Context, rows []map[string]any, cache *OwnerCache) {
	for _, row := range rows {
		row["is_remote"] = sessionIsRemote(ctx, row, cache)
	}
}

func sessionIsRemote(ctx context.Context, row map[string]any, cache *OwnerCache) bool {
	gitRef, ok := row["git_ref"].(map[string]any)
	if !ok {
		return false
	}
	wt, _ := gitRef["worktree_id"].(string)
	if wt == "" {
		return false
	}
	kind, ok, err := cache.Lookup(ctx, wt)
	if err != nil || !ok {
		return false
	}
	return kind == clanksync.OwnerKindRemote
}

// handleCreateSession routes POST /sessions to local or remote based
// on the request's Hostname (or, when empty, the OwnerCache view of
// the request's worktree). Body is read once and re-issued downstream.
func (g *Gateway) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	target := g.createTarget(r.Context(), body)
	switch target.kind {
	case clanksync.OwnerKindRemote:
		baseURL, jwt, ok := g.cfg.RemoteResolver.ActiveRemote()
		if !ok {
			http.Error(w, "remote-owned worktree but no active remote configured", http.StatusBadGateway)
			return
		}
		proxy, err := newRemoteReverseProxy(baseURL, jwt)
		if err != nil {
			g.log.Printf("gateway create-session: build remote proxy: %v", err)
			http.Error(w, "remote proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		r2 := cloneRequestWithBody(r, body)
		proxy.ServeHTTP(w, r2)
	default:
		// Local: rewind the body and fall through to the local-host
		// proxy. proxyToHost reads r.Body unchanged.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		g.proxyToHost(w, r)
	}
}

type createTarget struct {
	kind clanksync.OwnerKind
}

// createTarget decides where a POST /sessions should land. Peeks at
// the request's hostname and worktree_id; falls back to local on any
// ambiguity. Read-only on body (caller passes the buffered bytes).
func (g *Gateway) createTarget(ctx context.Context, body []byte) createTarget {
	var req struct {
		Hostname string `json:"hostname"`
		GitRef   struct {
			WorktreeID string `json:"worktree_id"`
		} `json:"git_ref"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return createTarget{kind: clanksync.OwnerKindLocal}
	}
	// Explicit hostname wins. "" or "local" → local; anything else →
	// remote (and the active remote profile is the destination —
	// multi-remote routing is out of scope here).
	if req.Hostname != "" && req.Hostname != "local" {
		return createTarget{kind: clanksync.OwnerKindRemote}
	}
	if req.GitRef.WorktreeID == "" || g.ownerCache == nil {
		return createTarget{kind: clanksync.OwnerKindLocal}
	}
	kind, ok, err := g.ownerCache.Lookup(ctx, req.GitRef.WorktreeID)
	if err != nil || !ok {
		return createTarget{kind: clanksync.OwnerKindLocal}
	}
	return createTarget{kind: kind}
}

// handlePerSession dispatches /sessions/{id}/... ops to local or
// remote based on the session's worktree's owner. Reverse-proxies in
// both cases so SSE (`/events`) and large bodies stream through.
func (g *Gateway) handlePerSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustPrincipal(r.Context()).UserID
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, "session id missing", http.StatusBadRequest)
		return
	}

	kind, err := g.ownerForSession(r.Context(), userID, sessionID)
	if errors.Is(err, ErrSessionUnknown) {
		http.Error(w, "session not in local clank-host — run `clank pull --migrate` to import", http.StatusNotFound)
		return
	}
	if err != nil {
		g.log.Printf("gateway per-session: %v", err)
		http.Error(w, "owner lookup: "+err.Error(), http.StatusBadGateway)
		return
	}

	if kind == clanksync.OwnerKindRemote {
		baseURL, jwt, ok := g.cfg.RemoteResolver.ActiveRemote()
		if !ok {
			http.Error(w, "session is remote-owned but no active remote is configured", http.StatusBadGateway)
			return
		}
		proxy, err := newRemoteReverseProxy(baseURL, jwt)
		if err != nil {
			g.log.Printf("gateway per-session: build remote proxy: %v", err)
			http.Error(w, "remote proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
		return
	}
	g.proxyToHost(w, r)
}

// fetchLocalUpstream runs an HTTP request against the local clank-host
// via the provisioner's HostRef. Used by the list/search decorators
// that need to inspect the response body, not just reverse-proxy.
func (g *Gateway) fetchLocalUpstream(ctx context.Context, userID, method, path string) (body []byte, contentType string, err error) {
	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		return nil, "", fmt.Errorf("ensure host: %w", err)
	}
	target := strings.TrimRight(ref.URL, "/") + path
	if !strings.HasPrefix(path, "/") {
		// Defensive — callers always pass an absolute path, but
		// double-slashing is worth guarding against.
		target = strings.TrimRight(ref.URL, "/") + "/" + path
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, "", fmt.Errorf("parse upstream URL %q: %w", target, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	cli := &http.Client{Transport: ref.Transport, Timeout: 10 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// cloneRequestWithBody returns a shallow copy of r with the body
// replaced by buf — used when we've already drained the original to
// peek at it and now want to reverse-proxy with the bytes intact.
func cloneRequestWithBody(r *http.Request, buf []byte) *http.Request {
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(buf))
	r2.ContentLength = int64(len(buf))
	r2.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	return r2
}

// Compile-time hint: agent.SessionInfo is the canonical row shape;
// we decode as map[string]any during decoration to keep the wire
// format extensible without locking the gateway to every field. This
// line keeps the import alive after a hypothetical refactor that
// removed our direct uses; remove with confidence if unused.
var _ = agent.SessionInfo{}
