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

// createWorktreeRequest is what mobile sends to POST /v1/worktrees/create.
// Mirrors the host's request shape one-for-one — the gateway is purely
// a routing edge here, not a transformer.
type createWorktreeRequest struct {
	BaseWorktreeID string `json:"base_worktree_id"`
	BaseBranch     string `json:"base_branch"`
}

// createWorktreeResponse mirrors host.CreateWorktreeResult. Defined
// locally rather than importing internal/host to keep the gateway
// dependency surface narrow.
type createWorktreeResponse struct {
	WorktreeID  string `json:"worktree_id"`
	Branch      string `json:"branch"`
	WorktreeDir string `json:"worktree_dir"`
	DisplayName string `json:"display_name"`
	OriginRepo  string `json:"origin_repo"`
}

// handleCreateWorktree services POST /v1/worktrees/create — the mobile
// app's entry point for creating a new worktree without leaving the
// device. The flow:
//
//  1. Authenticate the caller (Principal in context).
//  2. Proxy to the user's host POST /worktrees/create. The host does
//     the git work (worktree add, stamp the worktree-id file) and
//     computes the origin_repo.
//  3. Record the resulting row in the caller's worktrees DB so
//     subsequent GET /v1/worktrees calls see it. The host has already
//     generated the ULID; we insert that exact ID so the on-disk stamp
//     and the DB row agree.
//
// Refuses to serve when Sync is unconfigured (cloud sync-only mode has
// no host to talk to) — matches the 503 the migration routes return.
func (g *Gateway) handleCreateWorktree(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "create-worktree not available on this gateway (no Sync configured)", http.StatusServiceUnavailable)
		return
	}
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req createWorktreeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.BaseWorktreeID == "" {
		http.Error(w, "base_worktree_id is required", http.StatusBadRequest)
		return
	}
	if req.BaseBranch == "" {
		http.Error(w, "base_branch is required", http.StatusBadRequest)
		return
	}

	resp, err := g.callHostCreateWorktree(r.Context(), principal.UserID, req)
	if err != nil {
		g.log.Printf("gateway create-worktree: host call: %v", err)
		http.Error(w, "host: "+err.Error(), http.StatusBadGateway)
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
	if err := g.cfg.Sync.RegisterPrebuiltWorktree(r.Context(), row); err != nil {
		// The git worktree exists on the host but we couldn't persist
		// the row. Surface the error so the mobile user can retry; the
		// next register attempt with the same ID will collide (PK), so
		// a follow-up plan should add idempotency or rollback. For MVP
		// the typical failure mode is "transient DB hiccup" → retry.
		g.log.Printf("gateway create-worktree: store insert: %v", err)
		http.Error(w, "persist worktree", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListBranches proxies POST /v1/worktrees/list-branches straight
// to the user's host. Mobile sends `{git_ref: {worktree_id: ...}}`
// which the host already understands. No DB write — branches are
// computed on demand.
func (g *Gateway) handleListBranches(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Sync == nil {
		http.Error(w, "list-branches not available on this gateway (no Sync configured)", http.StatusServiceUnavailable)
		return
	}
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	ref, err := g.cfg.Provisioner.EnsureHost(r.Context(), principal.UserID)
	if err != nil {
		g.log.Printf("gateway list-branches: EnsureHost: %v", err)
		http.Error(w, "host unavailable", http.StatusBadGateway)
		return
	}

	target := strings.TrimRight(ref.URL, "/") + "/worktrees/list-branches"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build host request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	cli := &http.Client{Transport: ref.Transport, Timeout: 15 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		g.log.Printf("gateway list-branches: host call: %v", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// callHostCreateWorktree makes the POST /worktrees/create request to
// the user's host and decodes the response. Errors here surface as
// 502 to the mobile client.
func (g *Gateway) callHostCreateWorktree(ctx context.Context, userID string, in createWorktreeRequest) (createWorktreeResponse, error) {
	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		return createWorktreeResponse{}, fmt.Errorf("ensure host: %w", err)
	}

	buf, err := json.Marshal(in)
	if err != nil {
		return createWorktreeResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	target := strings.TrimRight(ref.URL, "/") + "/worktrees/create"
	if _, err := url.Parse(target); err != nil {
		return createWorktreeResponse{}, fmt.Errorf("invalid host URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		return createWorktreeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	cli := &http.Client{Transport: ref.Transport, Timeout: 30 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return createWorktreeResponse{}, fmt.Errorf("post host /worktrees/create: %w", err)
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
