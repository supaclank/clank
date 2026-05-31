package syncclient

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// CheckpointResult is the outcome of Client.PushCheckpoint.
type CheckpointResult struct {
	CheckpointID string
	Manifest     *checkpoint.Manifest
}

// RegisterWorktree registers a new worktree with clank-sync and returns
// the server-assigned ID. Callers should persist the ID locally and
// pass it to subsequent PushCheckpoint invocations for the same
// working directory. The sync server only accepts laptop-kind callers
// here; sprite-kind callers (X-Clank-Host-Id) get 403.
func (c *Client) RegisterWorktree(ctx context.Context, displayName string) (string, error) {
	if displayName == "" {
		return "", errors.New("syncclient: displayName is required")
	}

	body := map[string]string{
		"display_name": displayName,
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/v1/worktrees", body, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", errors.New("syncclient: server returned empty worktree id")
	}
	return resp.ID, nil
}

// PushCheckpoint runs the full checkpoint upload flow: build local
// bundles, request presigned URLs, upload each blob, commit. Cleans up
// the temp bundle files on return.
// baseCommit is the laptop's last-synced HEAD for this worktree (e.g.
// from a parity check); when the server lacks this checkpoint's HEAD but
// holds baseCommit's, only an incremental head bundle (HEAD ^base) is
// built and uploaded. "" ⇒ full bundle (or skipped if already stored).
func (c *Client) PushCheckpoint(ctx context.Context, worktreeID, repoPath, baseCommit string) (*CheckpointResult, error) {
	if worktreeID == "" {
		return nil, errors.New("syncclient: worktreeID is required")
	}

	// Generate a placeholder checkpoint ID for the bundle's temp refs;
	// the server assigns the canonical ID on /v1/checkpoints. We use
	// the server's ID as the manifest's CheckpointID at the end.
	tempID := "pending-" + randString(12)
	// CreatedBy is informational on the manifest — sync's CallerVerifier
	// derives the authoritative caller identity from the bearer.
	// Build only the cheap uncommitted bundle + manifest up front. The
	// head bundle (all history — slow to build AND upload) is built only
	// if the server doesn't already hold this HEAD, decided below.
	builder := checkpoint.NewBuilder(repoPath, "laptop")
	res, err := builder.BuildUncommitted(ctx, tempID)
	if err != nil {
		return nil, fmt.Errorf("build checkpoint: %w", err)
	}
	defer res.Cleanup()

	createReq := map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        res.Manifest.HeadCommit,
		"head_ref":           res.Manifest.HeadRef,
		"index_tree":         res.Manifest.IndexTree,
		"worktree_tree":      res.Manifest.WorktreeTree,
		"uncommitted_commit": res.Manifest.UncommittedCommit,
		"base_commit":        baseCommit,
	}
	var createResp struct {
		CheckpointID     string `json:"checkpoint_id"`
		HeadBundlePutURL string `json:"head_bundle_put_url"`
		HeadBundleBase   string `json:"head_bundle_base"`
		UncommittedURL   string `json:"uncommitted_put_url"`
		ManifestPutURL   string `json:"manifest_put_url"`
	}
	if err := c.postJSON(ctx, "/v1/checkpoints", createReq, &createResp); err != nil {
		// The create handler's only 404 is "worktree not registered" — a
		// stale local id for a worktree deleted on the remote. Surface it
		// typed so clank push can re-register and retry.
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w (id=%s)", ErrWorktreeNotRegistered, worktreeID)
		}
		return nil, err
	}

	// Stamp the server-assigned ID into the manifest before signing /
	// uploading. This makes the manifest self-describing on the server
	// side.
	res.Manifest.CheckpointID = createResp.CheckpointID

	// TODO(coderabbit): clean up server-side rows on partial-upload failure (abort endpoint or reaper)
	// https://github.com/Acksell/clank/pull/16
	//
	// Head bundle: build + upload ONLY when the server gave us a PUT URL.
	// An empty URL means the server already holds this HEAD's bundle
	// (content-addressed dedup) — the common idle-autopush case, where we
	// skip the slow build AND the slow upload entirely. HeadBundleBase is
	// "" for a full bundle; non-empty drives an incremental (Slice 2).
	//
	// Blob PUTs use blobClient (no ResponseHeaderTimeout): S3 returns the
	// PUT response only after the full body lands, so the control-plane
	// cap would abort any upload slower than 30s (e.g. a large bundle
	// over a tunnel).
	if createResp.HeadBundlePutURL != "" {
		headBundle, err := builder.BuildHeadBundle(ctx, tempID, res.Manifest.HeadCommit, createResp.HeadBundleBase)
		if err != nil {
			return nil, fmt.Errorf("build head bundle: %w", err)
		}
		defer os.Remove(headBundle)
		if err := uploadFile(ctx, c.blobClient, createResp.HeadBundlePutURL, headBundle); err != nil {
			return nil, fmt.Errorf("upload headCommit: %w", err)
		}
	}
	if err := uploadFile(ctx, c.blobClient, createResp.UncommittedURL, res.UncommittedBundle); err != nil {
		return nil, fmt.Errorf("upload uncommitted: %w", err)
	}
	manifestBytes, err := res.Manifest.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := uploadBytes(ctx, c.blobClient, createResp.ManifestPutURL, manifestBytes, "application/json"); err != nil {
		return nil, fmt.Errorf("upload manifest: %w", err)
	}

	// head_base records this HEAD's link in the server's chain (the base
	// the server told us to build from; "" for full / already_stored).
	if err := c.postJSON(ctx, "/v1/checkpoints/"+createResp.CheckpointID+"/commit", map[string]string{"head_base": createResp.HeadBundleBase}, nil); err != nil {
		return nil, fmt.Errorf("commit checkpoint: %w", err)
	}

	return &CheckpointResult{
		CheckpointID: createResp.CheckpointID,
		Manifest:     res.Manifest,
	}, nil
}

func (c *Client) postJSON(ctx context.Context, path string, body any, into any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &httpError{Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(respBody))}
	}
	if into != nil {
		if err := json.Unmarshal(respBody, into); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

func uploadFile(ctx context.Context, client *http.Client, url, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("PUT returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func uploadBytes(ctx context.Context, client *http.Client, url string, data []byte, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("PUT returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// randString returns a hex string of n bytes worth of randomness.
// Sufficient for the temp-ref namespace; not used for security.
func randString(n int) string {
	b := make([]byte, n)
	_, _ = cryptorand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0xf]
	}
	return string(out)
}
