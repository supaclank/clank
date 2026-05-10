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
// working directory.
func (c *Client) RegisterWorktree(ctx context.Context, displayName string) (string, error) {
	if c.cfg.DeviceID == "" {
		return "", errors.New("syncclient: DeviceID is required for the checkpoint flow")
	}
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
func (c *Client) PushCheckpoint(ctx context.Context, worktreeID, repoPath string) (*CheckpointResult, error) {
	if c.cfg.DeviceID == "" {
		return nil, errors.New("syncclient: DeviceID is required for the checkpoint flow")
	}
	if worktreeID == "" {
		return nil, errors.New("syncclient: worktreeID is required")
	}

	// Generate a placeholder checkpoint ID for the bundle's temp refs;
	// the server assigns the canonical ID on /v1/checkpoints. We use
	// the server's ID as the manifest's CheckpointID at the end.
	tempID := "pending-" + randString(12)
	builder := checkpoint.NewBuilder(repoPath, "laptop:"+c.cfg.DeviceID)
	res, err := builder.Build(ctx, tempID)
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
		"incremental_commit": res.Manifest.IncrementalCommit,
	}
	var createResp struct {
		CheckpointID     string `json:"checkpoint_id"`
		HeadCommitPutURL string `json:"head_commit_put_url"`
		IncrementalURL   string `json:"incremental_put_url"`
		ManifestPutURL   string `json:"manifest_put_url"`
	}
	if err := c.postJSON(ctx, "/v1/checkpoints", createReq, &createResp); err != nil {
		return nil, err
	}

	// Stamp the server-assigned ID into the manifest before signing /
	// uploading. This makes the manifest self-describing on the server
	// side.
	res.Manifest.CheckpointID = createResp.CheckpointID

	// TODO(coderabbit): clean up server-side rows on partial-upload failure (abort endpoint or reaper)
	// https://github.com/Acksell/clank/pull/16
	if err := uploadFile(ctx, c.client, createResp.HeadCommitPutURL, res.HeadCommitBundle); err != nil {
		return nil, fmt.Errorf("upload headCommit: %w", err)
	}
	if err := uploadFile(ctx, c.client, createResp.IncrementalURL, res.IncrementalBundle); err != nil {
		return nil, fmt.Errorf("upload incremental: %w", err)
	}
	manifestBytes, err := res.Manifest.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := uploadBytes(ctx, c.client, createResp.ManifestPutURL, manifestBytes, "application/json"); err != nil {
		return nil, fmt.Errorf("upload manifest: %w", err)
	}

	if err := c.postJSON(ctx, "/v1/checkpoints/"+createResp.CheckpointID+"/commit", map[string]string{}, nil); err != nil {
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
	if c.cfg.DeviceID != "" {
		// X-Clank-Device-Id is the laptop's identity for checkpoint
		// authorization. P2 will move this into JWT claims; for MVP it
		// rides as a header. Pinned in pkg/sync.HeaderDeviceID.
		req.Header.Set("X-Clank-Device-Id", c.cfg.DeviceID)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("post %s: %d: %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
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
