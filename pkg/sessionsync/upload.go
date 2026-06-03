package sessionsync

import (
	"context"
	"fmt"
	"time"

	"github.com/acksell/clank/pkg/sync/checkpoint"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// UploadSessions mints presigned PUT URLs for the exported sessions,
// uploads each blob, then uploads the session-manifest.json sidecar —
// all attached to checkpointID. An empty `exported` still uploads an
// empty manifest so the remote can tell "checkpoint with no sessions"
// from "pre-feature checkpoint".
//
// Daemon-free: the bytes go laptop → object storage directly via the
// gateway-minted URLs; nothing passes through a local clank-host.
//
// obs (nil-safe) receives per-session progress so the caller can render
// "(i/N)" while the blobs upload.
func UploadSessions(ctx context.Context, gateway *syncclient.Client, checkpointID string, exported []ExportedSession, obs syncclient.PushObserver) error {
	if checkpointID == "" {
		return fmt.Errorf("upload sessions: checkpointID is required")
	}

	sessionIDs := make([]string, len(exported))
	for i, e := range exported {
		sessionIDs[i] = e.Entry.SessionID
	}
	urls, err := gateway.RequestSessionUploadURLs(ctx, checkpointID, sessionIDs)
	if err != nil {
		return fmt.Errorf("request session upload URLs: %w", err)
	}

	total := len(exported)
	if obs != nil {
		obs.SessionProgress(0, total)
	}
	for i, e := range exported {
		putURL, ok := urls.SessionPutURLs[e.Entry.SessionID]
		if !ok {
			return fmt.Errorf("no upload URL for session %s", e.Entry.SessionID)
		}
		if err := gateway.PutFile(ctx, putURL, e.BlobPath); err != nil {
			return fmt.Errorf("upload session %s: %w", e.Entry.SessionID, err)
		}
		if obs != nil {
			obs.SessionProgress(i+1, total)
		}
	}

	entries := make([]checkpoint.SessionEntry, len(exported))
	for i, e := range exported {
		entries[i] = e.Entry
	}
	manifest := checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: checkpointID,
		Sessions:     entries,
		CreatedAt:    time.Now().UTC(),
		CreatedBy:    "laptop",
	}
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		return fmt.Errorf("marshal session manifest: %w", err)
	}
	if err := gateway.PutBytes(ctx, urls.SessionManifestPutURL, manifestBytes, "application/json"); err != nil {
		return fmt.Errorf("upload session manifest: %w", err)
	}
	return nil
}
