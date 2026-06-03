package sessionsync

import (
	"context"
	"fmt"
	"time"

	"github.com/acksell/clank/pkg/sync/checkpoint"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// UploadSessions ships a PushPlan to object storage, all attached to
// checkpointID: it mints presigned PUT URLs for the changed session blobs
// (plan.Upload), uploads each, then uploads the session-manifest.json
// sidecar built from the COMPLETE entry set (plan.Entries) — so the
// checkpoint stays a self-contained snapshot even though unchanged blobs
// weren't re-uploaded. An empty plan still uploads a (possibly empty)
// manifest so the remote can tell "checkpoint with no sessions" from
// "pre-feature checkpoint".
//
// Daemon-free: bytes go laptop → object storage directly via the
// gateway-minted URLs. obs (nil-safe) receives per-blob progress.
func UploadSessions(ctx context.Context, gateway *syncclient.Client, checkpointID string, plan *PushPlan, obs syncclient.PushObserver) error {
	if checkpointID == "" {
		return fmt.Errorf("upload sessions: checkpointID is required")
	}

	refs := make([]checkpoint.SessionBlobRef, len(plan.Upload))
	for i, e := range plan.Upload {
		refs[i] = e.Entry.BlobRef()
	}
	urls, err := gateway.RequestSessionUploadURLs(ctx, checkpointID, refs)
	if err != nil {
		return fmt.Errorf("request session upload URLs: %w", err)
	}

	total := len(plan.Upload)
	if obs != nil {
		obs.SessionProgress(0, total)
	}
	for i, e := range plan.Upload {
		putURL, ok := urls.SessionPutURLs[e.Entry.ExternalID]
		if !ok {
			return fmt.Errorf("no upload URL for session %s", e.Entry.ExternalID)
		}
		if err := gateway.PutFile(ctx, putURL, e.BlobPath); err != nil {
			return fmt.Errorf("upload session %s: %w", e.Entry.ExternalID, err)
		}
		if obs != nil {
			obs.SessionProgress(i+1, total)
		}
	}

	manifest := checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: checkpointID,
		Sessions:     plan.Entries,
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
