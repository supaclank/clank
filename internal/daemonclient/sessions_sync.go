package daemonclient

import (
	"context"
	"fmt"

	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// SessionBuildResult mirrors the response of POST /sync/sessions/build
// on clank-host. Entries describe each opencode session bundled in the
// build's temp files; Skipped lists sessions enumerated but not
// exported (currently: claude-code in v1).
type SessionBuildResult struct {
	BuildID string                    `json:"build_id"`
	Entries []checkpoint.SessionEntry `json:"entries"`
	Skipped []SkippedSession          `json:"skipped"`
}

// SkippedSession is the wire shape of host.SkippedSession. Duplicated
// here so the daemonclient package doesn't import internal/host.
type SkippedSession struct {
	SessionID string `json:"session_id"`
	Backend   string `json:"backend"`
	Reason    string `json:"reason"`
}

// BuildSessionCheckpoint asks clank-host (via the local daemon proxy)
// to quiesce + export every session in the worktree, returning the
// manifest entries + a build_id the caller uses to drive the upload
// step. The temp blobs are held on clank-host's disk until
// UploadSessionCheckpoint (or the 30-min reaper).
func (c *Client) BuildSessionCheckpoint(ctx context.Context, worktreeID, checkpointID string) (*SessionBuildResult, error) {
	if worktreeID == "" || checkpointID == "" {
		return nil, fmt.Errorf("BuildSessionCheckpoint: worktreeID and checkpointID are required")
	}
	body := map[string]string{
		"worktree_id":   worktreeID,
		"checkpoint_id": checkpointID,
	}
	var out SessionBuildResult
	if err := c.post(ctx, "/sync/sessions/build", body, &out); err != nil {
		return nil, fmt.Errorf("build session checkpoint: %w", err)
	}
	return &out, nil
}

// ApplySessionCheckpoint asks the local clank-host to import every
// session listed in the manifest at session_manifest_url, fetching
// each session blob via the per-session GET URLs in session_blob_urls.
// Mirrors the gateway → sprite session-apply call but addressed at the
// laptop's own clank-host (where the imported sessions need to land
// during pull --migrate).
func (c *Client) ApplySessionCheckpoint(ctx context.Context, worktreeID, manifestGetURL string, sessionBlobGetURLs map[string]string) error {
	if worktreeID == "" {
		return fmt.Errorf("ApplySessionCheckpoint: worktreeID is required")
	}
	if manifestGetURL == "" {
		// Empty manifest URL means the sprite had no sessions to push.
		// Not an error — just nothing to apply.
		return nil
	}
	body := map[string]any{
		"worktree_id":          worktreeID,
		"session_manifest_url": manifestGetURL,
		"session_blob_urls":    sessionBlobGetURLs,
	}
	if err := c.post(ctx, "/sync/sessions/apply-from-urls", body, nil); err != nil {
		return fmt.Errorf("apply session checkpoint: %w", err)
	}
	return nil
}

// UploadSessionCheckpoint asks clank-host to upload the per-session
// blobs + the session-manifest.json to the supplied presigned PUT
// URLs. On success clank-host removes the temp build files.
func (c *Client) UploadSessionCheckpoint(ctx context.Context, buildID, checkpointID string, sessionURLs map[string]string, manifestPutURL string) error {
	if buildID == "" {
		return fmt.Errorf("UploadSessionCheckpoint: buildID is required")
	}
	if checkpointID == "" || manifestPutURL == "" {
		return fmt.Errorf("UploadSessionCheckpoint: checkpointID and manifestPutURL are required")
	}
	body := map[string]any{
		"checkpoint_id":             checkpointID,
		"session_urls":              sessionURLs,
		"session_manifest_put_url":  manifestPutURL,
	}
	if err := c.post(ctx, "/sync/sessions/builds/"+buildID+"/upload", body, nil); err != nil {
		return fmt.Errorf("upload session checkpoint: %w", err)
	}
	return nil
}
