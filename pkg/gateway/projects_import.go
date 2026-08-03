package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/supaclank/clank/pkg/auth"
)

// importProjectRequest is what mobile sends to POST /v1/projects/import:
// the owner and name of an existing GitHub repo to clone. The clone URL
// is built host-side from the connected token — clients never send URLs.
type importProjectRequest struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
}

// hostImportProjectRequest is the body forwarded to the host's POST
// /projects/import.
type hostImportProjectRequest struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
}

// handleImportProject services POST /v1/projects/import — clone the
// caller's existing GitHub repo into a repo canonical + fresh worktree
// on the host. Mirrors handleCreateProject, but the host clones with
// its stored GitHub token (so private repos work) and keeps the origin
// remote.
//
// The host's response forwards verbatim, error or success: "GitHub not
// connected" (409) is an expected, client-actionable outcome the mobile
// app must distinguish from a generic upstream failure. The host
// filesystem is the repo registry, so there is nothing to record
// gateway-side.
func (g *Gateway) handleImportProject(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req importProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Owner == "" {
		http.Error(w, "owner is required", http.StatusBadRequest)
		return
	}
	if req.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}

	status, body, err := g.callHostImportProject(r.Context(), principal.UserID, hostImportProjectRequest{
		Owner:  req.Owner,
		Repo:   req.Repo,
		Branch: req.Branch,
	})
	if err != nil {
		g.log.Printf("gateway import-project: host call: %v", err)
		http.Error(w, "project import failed", http.StatusBadGateway)
		return
	}

	// Forward the host's response verbatim — typed errors (e.g. 409
	// github_not_connected) and the 201 CreateWorktreeResult alike.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// callHostImportProject makes the POST /projects/import request to the
// user's host. On a transport/encoding failure it returns a non-nil
// error; otherwise the host's (status, body) come back for the caller
// to forward verbatim.
func (g *Gateway) callHostImportProject(ctx context.Context, userID string, in hostImportProjectRequest) (int, []byte, error) {
	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		return 0, nil, fmt.Errorf("ensure host: %w", err)
	}

	buf, err := json.Marshal(in)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	target := strings.TrimRight(ref.URL, "/") + "/projects/import"
	if _, err := url.Parse(target); err != nil {
		return 0, nil, fmt.Errorf("invalid host URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Cloning is a network operation on the host; match create-project's
	// generous timeout.
	cli := &http.Client{Transport: ref.Transport, Timeout: 120 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post host /projects/import: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}
