package syncclient

import (
	"context"
	"errors"

	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// SessionUploadURLs is the result of Client.RequestSessionUploadURLs.
// SessionPutURLs is keyed by externalID (SessionEntry.ExternalID).
type SessionUploadURLs struct {
	CheckpointID          string
	SessionPutURLs        map[string]string
	SessionManifestPutURL string
}

// RequestSessionUploadURLs asks the sync server for presigned PUT
// URLs covering a checkpoint's content-addressed session blobs + the
// session-manifest.json sidecar. The checkpoint must already exist
// (typically just created by PushCheckpoint).
//
// refs is the explicit set of session blobs to mint per-session URLs
// for (externalID + contentHash), derived from the SessionManifest the
// caller is about to upload. An empty slice still mints a
// session-manifest.json URL — useful when uploading an empty manifest
// for a worktree with no sessions.
//
// sessionsContentDigest is the manifest's ContentDigest over the COMPLETE
// session set (not just refs); the server persists it on the checkpoint row
// so autosync can skip the manifest fetch when unchanged. Empty is allowed
// (the server then leaves the digest unset).
func (c *Client) RequestSessionUploadURLs(ctx context.Context, checkpointID string, refs []checkpoint.SessionBlobRef, sessionsContentDigest string) (*SessionUploadURLs, error) {
	if checkpointID == "" {
		return nil, errors.New("syncclient: checkpointID is required")
	}
	body := map[string]any{
		"sessions":                refs,
		"sessions_content_digest": sessionsContentDigest,
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
