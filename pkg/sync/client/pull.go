package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PullHeadBundle is one link of the head chain returned by a pull: a
// presigned GET for the head bundle ending at TipSHA, built from BaseSHA
// ("" = full baseline).
type PullHeadBundle struct {
	TipSHA  string `json:"tip_sha"`
	BaseSHA string `json:"base_sha,omitempty"`
	GetURL  string `json:"get_url"`
}

// PullResult is the materialized sandbox state the gateway returns for a
// pull: the ordered head-bundle chain + presigned GETs for the code
// manifest and uncommitted bundle, plus the opencode session manifest +
// per-session blob URLs. The laptop downloads these and applies them
// locally (see the clank CLI's applyRemotePull).
type PullResult struct {
	CheckpointID string `json:"checkpoint_id"`
	ManifestURL  string `json:"manifest_url"`
	// HeadBundles is ordered oldest→newest; fetch+apply in order.
	HeadBundles        []PullHeadBundle  `json:"head_bundles"`
	UncommittedURL     string            `json:"uncommitted_url"`
	SessionManifestURL string            `json:"session_manifest_url,omitempty"`
	SessionBlobURLs    map[string]string `json:"session_blob_urls,omitempty"`
}

// PullWorktree asks the gateway to materialize the sandbox's current
// state for worktreeID: wake the sandbox, quiesce its sessions, build
// the code + session bundles, upload them to object storage, and return
// presigned GET URLs. haveHead is the laptop's current HEAD, so the
// gateway returns only the head-chain slice the laptop is missing.
//
// This is a long-running call — a cold sandbox wake plus build can take
// minutes before the first response byte — so it bounds the request with
// ctx rather than a client-level timeout, and drops the default
// ResponseHeaderTimeout that the push/presign calls rely on.
func (c *Client) PullWorktree(ctx context.Context, worktreeID, haveHead string) (*PullResult, error) {
	if worktreeID == "" {
		return nil, errors.New("syncclient: worktreeID is required")
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/worktrees/" + worktreeID + "/pull"
	if haveHead != "" {
		url += "?have_head=" + haveHead
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	if c.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}

	resp, err := c.blobClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pull worktree: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("pull worktree: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out PullResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode pull response: %w", err)
	}
	// An empty HeadBundles is legitimate: an incremental pull where the
	// laptop already has the sandbox's HEAD commit and only uncommitted
	// work changed. The manifest + uncommitted bundle are always present.
	if out.ManifestURL == "" || out.UncommittedURL == "" {
		return nil, fmt.Errorf("pull worktree: incomplete response (missing bundle URLs)")
	}
	return &out, nil
}
