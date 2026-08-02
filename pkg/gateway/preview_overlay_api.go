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

	"github.com/acksell/clank/internal/webpreview"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/tokens"
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
	if method == http.MethodPost && path == "/sessions" {
		return true
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
