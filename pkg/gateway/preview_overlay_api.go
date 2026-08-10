package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/supaclank/clank/internal/webpreview"
	"github.com/supaclank/clank/pkg/preview/routestore"
	"github.com/supaclank/clank/pkg/preview/tokens"
)

// authorizeOverlayAPI is deliberately stricter than preview visibility. A
// public link grants view access to the app, never control of its agent.
func (s *previewState) authorizeOverlayAPI(
	w http.ResponseWriter,
	r *http.Request,
	route routestore.Route,
	token string,
) bool {
	if len(s.signingKey) > 0 && tokens.VerifyFromRequest(s.signingKey, token, r, s.now()) == nil {
		return true
	}
	principal, err := s.auth.Verify(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="clank-preview-overlay"`)
		http.Error(w, "owner authentication required", http.StatusUnauthorized)
		return false
	}
	if principal.UserID != route.OwnerUserID {
		http.NotFound(w, r)
		return false
	}
	return true
}

// serveOverlayAPI relays the browser overlay to clank-host's control plane.
// The owner gate runs first; path validation scopes it to the route's worktree.
func (s *previewState) serveOverlayAPI(w http.ResponseWriter, r *http.Request, route routestore.Route) {
	apiPath := strings.TrimPrefix(r.URL.Path, webpreview.APIPrefix)
	if apiPath == "" {
		apiPath = "/"
	}
	if !overlayAPIPathAllowed(r.Method, apiPath) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && apiPath == "/config-options" {
		// The probe opens a short-lived agent in the requested worktree.
		// Hosted previews must never turn that into a path oracle or let an
		// owner-scoped overlay probe another route's checkout.
		q := r.URL.Query()
		if q.Get("git_worktree_id") != route.WorktreeID || q.Get("git_local_path") != "" {
			http.Error(w, "config options worktree does not match preview", http.StatusForbidden)
			return
		}
	}
	if r.Method == http.MethodPost && apiPath == "/sessions" {
		if !validateOverlaySessionCreate(w, r, route.WorktreeID) {
			return
		}
	}
	// Source-control routes are keyed by worktree id in the path; an
	// owner-scoped overlay must never reach past its route's checkout.
	if worktreeID, ok := overlayWorktreeID(apiPath); ok && worktreeID != route.WorktreeID {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost && apiPath == "/worktrees/list-branches" {
		if !validateOverlayWorktreeRef(w, r, route.WorktreeID) {
			return
		}
	}

	ref, err := s.gw.cfg.Provisioner.GetHostByID(r.Context(), route.HostID)
	if err != nil {
		s.log.Printf("preview overlay API: resolve host %s: %v", route.HostID, err)
		http.Error(w, "host unavailable", http.StatusBadGateway)
		return
	}
	target, err := url.Parse(ref.URL)
	if err != nil {
		s.log.Printf("preview overlay API: invalid host URL %q: %v", ref.URL, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	if sessionID, ok := overlaySessionID(apiPath); ok {
		belongs, lookupErr := overlaySessionBelongsToWorktree(
			r.Context(), ref.Transport, target, sessionID, route.WorktreeID,
		)
		if lookupErr != nil {
			s.log.Printf("preview overlay API: session scope lookup %s: %v", sessionID, lookupErr)
			http.Error(w, "session unavailable", http.StatusBadGateway)
			return
		}
		if !belongs {
			http.NotFound(w, r)
			return
		}
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.URL.Path = singleJoiningSlash(target.Path, apiPath)
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-Clank-Token")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Proto")
		},
		Transport:     ref.Transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
			s.log.Printf("preview overlay API: upstream error on %s %s: %v", req.Method, req.URL.Path, proxyErr)
			http.Error(rw, "overlay_api_upstream_error", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

func overlayAPIPathAllowed(method, path string) bool {
	if method == http.MethodGet && (path == "/backends" || path == "/presets" || path == "/config-options") {
		return true
	}
	if method == http.MethodPost && path == "/presets" {
		return true
	}
	if method == http.MethodPost && path == "/sessions" {
		return true
	}
	// GitHub connection status + device-flow connect. The overlay API
	// already grants agent control (arbitrary code on the host), so a
	// GitHub connect initiated here adds no authority beyond that.
	if method == http.MethodGet && (path == "/credentials/github/status" || path == "/credentials/github/connect/status") {
		return true
	}
	if method == http.MethodPost && (path == "/credentials/github/connect/start" || path == "/credentials/github/connect/cancel") {
		return true
	}
	// Branch listing (default branch + diff stats for the source-control
	// chip). Body git_ref is scoped in serveOverlayAPI.
	if method == http.MethodPost && path == "/worktrees/list-branches" {
		return true
	}
	if _, suffix, ok := overlayWorktreeRoute(path); ok {
		switch {
		case method == http.MethodGet && suffix == "/remote/status":
			return true
		case method == http.MethodPost && (suffix == "/remote/push" || suffix == "/remote/pull" ||
			suffix == "/remote/resolve" || suffix == "/remote/publish"):
			return true
		case method == http.MethodPost && (suffix == "/pr" || suffix == "/pr/preview" || suffix == "/pr/ready"):
			return true
		default:
			return false
		}
	}
	_, suffix, ok := overlaySessionRoute(path)
	if !ok {
		return false
	}
	switch {
	case method == http.MethodGet && (suffix == "" || suffix == "/messages" || suffix == "/events" || suffix == "/pending-permission"):
		return true
	case method == http.MethodPost && (suffix == "/message" || suffix == "/abort" || suffix == "/revert"):
		return true
	case method == http.MethodPost && strings.HasPrefix(suffix, "/permissions/") && strings.HasSuffix(suffix, "/reply"):
		return true
	case method == http.MethodPost && strings.HasPrefix(suffix, "/questions/") && strings.HasSuffix(suffix, "/reply"):
		return true
	default:
		return false
	}
}

func validateOverlaySessionCreate(w http.ResponseWriter, r *http.Request, worktreeID string) bool {
	const maxCreateBodyBytes = 32 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCreateBodyBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return false
	}
	if len(body) > maxCreateBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		Backend string `json:"backend"`
		GitRef  struct {
			WorktreeID string `json:"worktree_id"`
		} `json:"git_ref"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid session request", http.StatusBadRequest)
		return false
	}
	if req.Backend == "" {
		http.Error(w, "backend is required", http.StatusBadRequest)
		return false
	}
	if req.GitRef.WorktreeID != worktreeID {
		http.Error(w, "session worktree does not match preview", http.StatusForbidden)
		return false
	}
	return true
}

// overlayWorktreeRoute parses /worktrees/{id}/<suffix>. Body-addressed
// routes like /worktrees/list-branches have no per-id segment and do
// not match (their scoping is body-based, see validateOverlayWorktreeRef).
func overlayWorktreeRoute(path string) (worktreeID, suffix string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/worktrees/")
	if !ok {
		return "", "", false
	}
	worktreeID, tail, hasTail := strings.Cut(rest, "/")
	if worktreeID == "" || !hasTail {
		return "", "", false
	}
	decoded, err := url.PathUnescape(worktreeID)
	if err != nil || decoded == "" {
		return "", "", false
	}
	return decoded, "/" + tail, true
}

func overlayWorktreeID(path string) (string, bool) {
	worktreeID, _, ok := overlayWorktreeRoute(path)
	return worktreeID, ok
}

// validateOverlayWorktreeRef enforces that a body-addressed git_ref
// stays inside the preview's worktree — the same containment
// validateOverlaySessionCreate applies to session creation.
func validateOverlayWorktreeRef(w http.ResponseWriter, r *http.Request, worktreeID string) bool {
	const maxRefBodyBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRefBodyBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return false
	}
	if len(body) > maxRefBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		GitRef struct {
			WorktreeID string `json:"worktree_id"`
			LocalPath  string `json:"local_path"`
		} `json:"git_ref"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	if req.GitRef.LocalPath != "" {
		http.Error(w, "git ref worktree does not match preview", http.StatusForbidden)
		return false
	}
	if req.GitRef.WorktreeID == "" {
		http.Error(w, "git_ref.worktree_id is required", http.StatusBadRequest)
		return false
	}
	if req.GitRef.WorktreeID != worktreeID {
		http.Error(w, "git ref worktree does not match preview", http.StatusForbidden)
		return false
	}
	return true
}

func overlaySessionID(path string) (string, bool) {
	sessionID, _, ok := overlaySessionRoute(path)
	return sessionID, ok
}

func overlaySessionRoute(path string) (sessionID, suffix string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/sessions/")
	if !ok {
		return "", "", false
	}
	sessionID, tail, hasTail := strings.Cut(rest, "/")
	if sessionID == "" {
		return "", "", false
	}
	decoded, err := url.PathUnescape(sessionID)
	if err != nil || decoded == "" {
		return "", "", false
	}
	if hasTail {
		suffix = "/" + tail
	}
	return decoded, suffix, true
}

func overlaySessionBelongsToWorktree(
	ctx context.Context,
	transport http.RoundTripper,
	target *url.URL,
	sessionID string,
	worktreeID string,
) (bool, error) {
	lookup := *target
	lookup.Path = singleJoiningSlash(target.Path, "/sessions/"+url.PathEscape(sessionID))
	lookup.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookup.String(), nil)
	if err != nil {
		return false, err
	}
	req.Host = target.Host
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("session lookup status %d", resp.StatusCode)
	}
	var info struct {
		GitRef struct {
			WorktreeID string `json:"worktree_id"`
		} `json:"git_ref"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return false, err
	}
	return info.GitRef.WorktreeID == worktreeID, nil
}
