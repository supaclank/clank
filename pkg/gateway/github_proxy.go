package gateway

// Shared helper for the GitHub Connect proxy endpoints. Each
// handler in github_status.go / github_disconnect.go /
// github_connect.go / github_pr.go funnels through proxyHostGitHub
// so the EnsureHost + transport + status-copy plumbing is in one
// place. Mirrors the shape of handleListBranches at
// pkg/gateway/worktrees_create.go without the Sync==nil refusal —
// these endpoints work in laptop mode too.

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
)

// hostGitHubTimeout caps the round-trip to the user's host for any
// GitHub-proxy call. Generous enough for cold-sprite wake; tight
// enough that a hung host doesn't block mobile UI indefinitely.
const hostGitHubTimeout = 30 * time.Second

// proxyHostGitHub forwards r to <hostURL><hostPath><?rawQuery>.
// Preserves the request method, copies the body, the response
// status, the JSON content-type, and the response body verbatim.
//
// Returns false when authentication or host resolution failed —
// the caller's handler should return immediately in that case (the
// response has already been written).
func (g *Gateway) proxyHostGitHub(w http.ResponseWriter, r *http.Request, hostPath string) bool {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return false
	}

	ref, err := g.cfg.Provisioner.EnsureHost(r.Context(), principal.UserID)
	if err != nil {
		g.log.Printf("gateway github %s: EnsureHost: %v", hostPath, err)
		http.Error(w, "host unavailable", http.StatusBadGateway)
		return false
	}

	target := strings.TrimRight(ref.URL, "/") + hostPath
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build host request: "+err.Error(), http.StatusInternalServerError)
		return false
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	cli := &http.Client{Transport: ref.Transport, Timeout: hostGitHubTimeout}
	resp, err := cli.Do(req)
	if err != nil {
		g.log.Printf("gateway github %s: host call: %v", hostPath, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return false
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}
