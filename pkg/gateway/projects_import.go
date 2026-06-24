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

	"github.com/acksell/clank/pkg/auth"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// importProjectRequest is what mobile sends to POST /v1/projects/import:
// the owner and name of an existing GitHub repo to clone. The clone URL
// is built host-side from the connected token — clients never send URLs.
type importProjectRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// hostImportProjectRequest is the body forwarded to the host's POST
// /projects/import.
type hostImportProjectRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// handleImportProject services POST /v1/projects/import — clone the
// caller's existing GitHub repo into a fresh worktree. Mirrors
// handleCreateProject, but the host clones with its stored GitHub token
// (so private repos work) and keeps the origin remote.
//
// Unlike create-project, host error responses are forwarded verbatim:
// "GitHub not connected" (409) is an expected, client-actionable outcome
// the mobile app must distinguish from a generic upstream failure.
//
// Refuses when Sync is unconfigured: Sync is the worktree registry, so
// without it there's nowhere to record the imported project.
func (g *Gateway) handleImportProject(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "import-project not available on this gateway (no Sync configured)", http.StatusServiceUnavailable)
		return
	}
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

	resp, status, body, err := g.callHostImportProject(r.Context(), principal.UserID, hostImportProjectRequest{
		Owner: req.Owner,
		Repo:  req.Repo,
	})
	if err != nil {
		g.log.Printf("gateway import-project: host call: %v", err)
		http.Error(w, "project import failed", http.StatusBadGateway)
		return
	}
	if status/100 != 2 {
		// Forward the host's typed error (e.g. 409 github_not_connected)
		// so the client can react precisely.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	now := time.Now().UTC()
	row := clanksync.Worktree{
		ID:          resp.WorktreeID,
		UserID:      principal.UserID,
		DisplayName: resp.DisplayName,
		OriginRepo:  resp.OriginRepo,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// TODO(ai-review): if RegisterPrebuiltWorktree fails the host project dir is orphaned;
	// add a best-effort DELETE /worktrees/{id} rollback call (shared with create-project).
	if err := g.cfg.Sync.RegisterPrebuiltWorktree(r.Context(), row); err != nil {
		g.log.Printf("gateway import-project: store insert: %v", err)
		http.Error(w, "persist project", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// callHostImportProject makes the POST /projects/import request to the
// user's host. On a transport/encoding failure it returns a non-nil
// error. On an HTTP response it returns the status and raw body so the
// caller can forward non-2xx (e.g. github_not_connected) verbatim;
// for 2xx it also decodes resp. Reuses createWorktreeResponse — the host
// returns the same CreateWorktreeResult shape.
func (g *Gateway) callHostImportProject(ctx context.Context, userID string, in hostImportProjectRequest) (createWorktreeResponse, int, []byte, error) {
	var out createWorktreeResponse

	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		return out, 0, nil, fmt.Errorf("ensure host: %w", err)
	}

	buf, err := json.Marshal(in)
	if err != nil {
		return out, 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	target := strings.TrimRight(ref.URL, "/") + "/projects/import"
	if _, err := url.Parse(target); err != nil {
		return out, 0, nil, fmt.Errorf("invalid host URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		return out, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Cloning is a network operation on the host; match create-project's
	// generous timeout.
	cli := &http.Client{Transport: ref.Transport, Timeout: 120 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return out, 0, nil, fmt.Errorf("post host /projects/import: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return out, resp.StatusCode, body, nil
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, resp.StatusCode, body, fmt.Errorf("decode host response: %w", err)
	}
	if out.WorktreeID == "" {
		return out, resp.StatusCode, body, errors.New("host returned empty worktree_id")
	}
	return out, resp.StatusCode, body, nil
}
