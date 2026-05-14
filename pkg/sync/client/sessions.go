package syncclient

import (
	"context"
	"errors"
)

// SessionUploadURLs is the result of Client.RequestSessionUploadURLs.
// SessionPutURLs is keyed by host ULID (SessionEntry.SessionID).
type SessionUploadURLs struct {
	CheckpointID          string
	SessionPutURLs        map[string]string
	SessionManifestPutURL string
}

// RequestSessionUploadURLs asks the sync server for presigned PUT
// URLs covering a checkpoint's per-session export blobs + the
// session-manifest.json sidecar. The checkpoint must already exist
// (typically just created by PushCheckpoint).
//
// sessionIDs is the explicit set of host ULIDs to mint per-session
// URLs for, derived from the SessionManifest the caller is about to
// upload. An empty slice still mints a session-manifest.json URL —
// useful when uploading an empty manifest for a worktree with no
// sessions.
func (c *Client) RequestSessionUploadURLs(ctx context.Context, checkpointID string, sessionIDs []string) (*SessionUploadURLs, error) {
	if checkpointID == "" {
		return nil, errors.New("syncclient: checkpointID is required")
	}
	body := map[string]any{
		"session_ids": sessionIDs,
	}
	var resp struct {
		CheckpointID          string            `json:"checkpoint_id"`
		SessionPutURLs        map[string]string `json:"session_put_urls"`
		SessionManifestPutURL string            `json:"session_manifest_put_url"`
	}
	if err := c.postJSON(ctx, "/v1/checkpoints/"+checkpointID+"/sessions", body, &resp); err != nil {
		return nil, err
	}
	if resp.SessionManifestPutURL == "" {
		return nil, errors.New("syncclient: server returned empty session_manifest_put_url")
	}
	return &SessionUploadURLs{
		CheckpointID:          resp.CheckpointID,
		SessionPutURLs:        resp.SessionPutURLs,
		SessionManifestPutURL: resp.SessionManifestPutURL,
	}, nil
}
