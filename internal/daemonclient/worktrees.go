package daemonclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// WorktreeInfo mirrors the JSON shape of pkg/sync's worktreeResponse,
// duplicated here so daemonclient stays decoupled from the gateway
// types. Fields are a strict subset — daemonclient consumers (the TUI
// sidebar today) only need identity + ownership.
type WorktreeInfo struct {
	ID                       string              `json:"id"`
	UserID                   string              `json:"user_id"`
	DisplayName              string              `json:"display_name"`
	OwnerKind                string              `json:"owner_kind"`
	OwnerID                  string              `json:"owner_id"`
	LatestSyncedCheckpoint   string              `json:"latest_synced_checkpoint,omitempty"`
	LatestCheckpointMetadata *CheckpointMetadata `json:"latest_checkpoint_metadata,omitempty"`
}

// CheckpointMetadata is the 4-SHA snapshot the laptop uses for cheap
// divergence detection. Returned only on single-worktree responses
// (GET /v1/worktrees/{id}, POST /v1/worktrees/{id}/owner); empty on
// list endpoints to avoid a JOIN per row.
type CheckpointMetadata struct {
	CheckpointID      string `json:"checkpoint_id"`
	HeadCommit        string `json:"head_commit"`
	HeadRef           string `json:"head_ref,omitempty"`
	IndexTree         string `json:"index_tree"`
	WorktreeTree      string `json:"worktree_tree"`
	IncrementalCommit string `json:"incremental_commit"`
}

// GetWorktree fetches a single worktree row, including the latest
// checkpoint's content-SHA snapshot. The snapshot is what `clank
// push`/`pull` compare local state against to decide "up to date vs
// diverged".
func (c *Client) GetWorktree(ctx context.Context, id string) (*WorktreeInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/worktrees/"+id, nil)
	if err != nil {
		return nil, err
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get worktree: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read get worktree: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrWorktreeNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get worktree: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var wt WorktreeInfo
	if err := json.Unmarshal(body, &wt); err != nil {
		return nil, fmt.Errorf("decode get worktree: %w", err)
	}
	return &wt, nil
}

// ErrWorktreeNotFound is returned by GetWorktree when the remote sync
// server has no row matching the requested ID. Callers map this to a
// "this worktree isn't registered with the active remote" hint.
var ErrWorktreeNotFound = fmt.Errorf("daemonclient: worktree not found on remote")

// ListWorktrees returns the active remote's worktrees. Routes through
// GET /v1/worktrees on the gateway's embedded sync server; only
// makes sense against a remote-mode client (TCP, with Sync configured
// upstream). Returns an empty slice for local-only daemons.
func (c *Client) ListWorktrees(ctx context.Context) ([]WorktreeInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/worktrees", nil)
	if err != nil {
		return nil, err
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read list worktrees: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		// Local-only daemon (Sync=nil) doesn't mount the route. Treat
		// as "no worktree metadata available" rather than an error so
		// the sidebar can gracefully omit ownership glyphs.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list worktrees: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Worktrees []WorktreeInfo `json:"worktrees"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode list worktrees: %w", err)
	}
	return parsed.Worktrees, nil
}
