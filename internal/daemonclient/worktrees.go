package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/acksell/clank/internal/cloud"
)

// OwnerKind enumerates which actor type owns a worktree's write
// authority. Mirrors pkg/sync.OwnerKind so daemonclient callers don't
// have to import pkg/sync just to spell a string. Per the project's
// "no magic strings" rule, never compare WorktreeInfo.OwnerKind
// against raw "local"/"remote" literals at call sites — use these.
type OwnerKind string

const (
	OwnerKindLocal  OwnerKind = "local"
	OwnerKindRemote OwnerKind = "remote"
)

// ErrOwnerConflict is returned by ReclaimWorktree when the server's
// optimistic-concurrency guard rejects the claim because the row's
// current OwnerID no longer matches the caller's expected_owner_id —
// someone else reclaimed first, or the caller's view is stale. Callers
// should re-read the worktree and decide whether to retry.
var ErrOwnerConflict = errors.New("daemonclient: worktree owner changed under us — re-read and retry")

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
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("get worktree: %w", cloud.ErrUnauthorized)
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
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("list worktrees: %w", cloud.ErrUnauthorized)
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

// ReclaimWorktree atomically claims local ownership of a worktree the
// caller does NOT currently own — the "new owner claims" path of
// POST /v1/worktrees/{id}/owner.
//
// Used by `clank push -m --discard-remote` to take a remote-owned
// worktree back before pushing the laptop's state. The transfer is
// metadata-only on the server (a single SQL UPDATE); no bundles are
// downloaded — sprite's pending checkpoint is orphaned, which is
// exactly what "discard remote" means at the data level.
//
// expectedOwnerID is the caller's read of the row's current OwnerID
// (from a recent GetWorktree). The server's optimistic-concurrency
// guard returns ErrOwnerConflict if it has changed under us.
func (c *Client) ReclaimWorktree(ctx context.Context, worktreeID, expectedOwnerID string) (*WorktreeInfo, error) {
	if worktreeID == "" {
		return nil, fmt.Errorf("ReclaimWorktree: worktreeID is required")
	}

	body, err := json.Marshal(map[string]string{
		"to_kind":           string(OwnerKindLocal),
		"to_id":             "",
		"expected_owner_id": expectedOwnerID,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/worktrees/"+worktreeID+"/owner", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reclaim worktree: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read reclaim response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var wt WorktreeInfo
		if err := json.Unmarshal(respBody, &wt); err != nil {
			return nil, fmt.Errorf("decode reclaim response: %w", err)
		}
		return &wt, nil
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("reclaim worktree: %w", cloud.ErrUnauthorized)
	case http.StatusNotFound:
		return nil, ErrWorktreeNotFound
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", ErrOwnerConflict, strings.TrimSpace(string(respBody)))
	default:
		return nil, fmt.Errorf("reclaim worktree: %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
