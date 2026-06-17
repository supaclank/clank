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

// createProjectRequest is what mobile sends to POST /v1/projects/create.
// template is a catalog id (see GET /v1/templates); the gateway resolves
// it to a clone URL — clients never send URLs.
type createProjectRequest struct {
	Template string `json:"template"`
	Name     string `json:"name"`
}

// hostCreateProjectRequest is the body the gateway sends to the host's
// POST /projects/create. The host gets the resolved clone_url, never the
// catalog id.
type hostCreateProjectRequest struct {
	CloneURL string `json:"clone_url"`
	Name     string `json:"name"`
}

// handleCreateProject services POST /v1/projects/create — the mobile
// app's entry point for scaffolding a brand-new project (no existing
// repo). The flow:
//
//  1. Authenticate the caller (Principal in context).
//  2. Resolve the requested template id to a clone URL from the
//     configured catalog. Unknown id → 404; this is the only place
//     template ids are interpreted.
//  3. Proxy to the user's host POST /projects/create with the clone URL.
//     The host clones the template into a fresh ~/work worktree, drops
//     the template history, re-inits a remote-less repo, and stamps the
//     worktree-id.
//  4. Record the resulting row in the caller's worktrees DB so
//     subsequent GET /v1/worktrees calls see it — same as worktree
//     creation; we insert the exact ULID the host minted.
//
// Refuses when Sync is unconfigured (no host to talk to / nowhere to
// persist the row) — matches the create-worktree route.
func (g *Gateway) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "create-project not available on this gateway (no Sync configured)", http.StatusServiceUnavailable)
		return
	}
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Template == "" {
		http.Error(w, "template is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	cloneURL, ok := g.cloneURLForTemplate(req.Template)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown template %q", req.Template), http.StatusNotFound)
		return
	}

	resp, err := g.callHostCreateProject(r.Context(), principal.UserID, hostCreateProjectRequest{
		CloneURL: cloneURL,
		Name:     req.Name,
	})
	if err != nil {
		g.log.Printf("gateway create-project: host call: %v", err)
		http.Error(w, "project creation failed", http.StatusBadGateway)
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
	// add a best-effort DELETE /worktrees/{id} rollback call. https://github.com/Acksell/clank/pull/61#discussion_r3428655410
	if err := g.cfg.Sync.RegisterPrebuiltWorktree(r.Context(), row); err != nil {
		// The project exists on the host but the row didn't persist.
		// Surface the error so the user can retry (same caveat as
		// create-worktree: a retry collides on the PK until rollback/
		// idempotency lands).
		g.log.Printf("gateway create-project: store insert: %v", err)
		http.Error(w, "persist project", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// callHostCreateProject makes the POST /projects/create request to the
// user's host and decodes the response. Errors here surface as 502 to
// the mobile client. Reuses createWorktreeResponse — the host returns
// the same CreateWorktreeResult shape.
func (g *Gateway) callHostCreateProject(ctx context.Context, userID string, in hostCreateProjectRequest) (createWorktreeResponse, error) {
	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		return createWorktreeResponse{}, fmt.Errorf("ensure host: %w", err)
	}

	buf, err := json.Marshal(in)
	if err != nil {
		return createWorktreeResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	target := strings.TrimRight(ref.URL, "/") + "/projects/create"
	if _, err := url.Parse(target); err != nil {
		return createWorktreeResponse{}, fmt.Errorf("invalid host URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		return createWorktreeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Cloning a template is a network operation on the host; give it more
	// headroom than the worktree-create call's 30s.
	cli := &http.Client{Transport: ref.Transport, Timeout: 120 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return createWorktreeResponse{}, fmt.Errorf("post host /projects/create: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return createWorktreeResponse{}, fmt.Errorf("host returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out createWorktreeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return createWorktreeResponse{}, fmt.Errorf("decode host response: %w", err)
	}
	if out.WorktreeID == "" {
		return createWorktreeResponse{}, errors.New("host returned empty worktree_id")
	}
	return out, nil
}
