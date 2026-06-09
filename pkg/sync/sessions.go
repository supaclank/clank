package sync

import (
	"context"
	"fmt"

	"github.com/acksell/clank/pkg/sync/checkpoint"
	"github.com/acksell/clank/pkg/sync/storage"
)

// SessionPresignRequest is the input for Server.PresignSessionPuts. It
// rides on top of an existing checkpoint: the code checkpoint must
// already exist (typically just created by CreateCheckpoint) before
// sessions can be added. Sessions carries one content-addressed ref per
// blob to presign.
type SessionPresignRequest struct {
	CheckpointID string
	Sessions     []checkpoint.SessionBlobRef
	// SessionsContentDigest is the manifest's content-digest
	// (checkpoint.SessionManifest.ContentDigest) for the session set being
	// uploaded — computed by the client, which already holds the same
	// (ExternalID, ContentHash) pairs the digest hashes. Persisted on the
	// checkpoint row so autosync can skip the S3 manifest fetch when it
	// matches what the sprite imported. Empty ⇒ not persisted (old clients
	// or a code-only push).
	SessionsContentDigest string
}

// SessionPresignResult carries the per-session PUT URLs + the
// session-manifest.json PUT URL. SessionPutURLs is keyed by externalID
// (the manifest's SessionEntry.ExternalID).
type SessionPresignResult struct {
	CheckpointID          string
	SessionPutURLs        map[string]string
	SessionManifestPutURL string
}

// PresignSessionPuts mints presigned PUT URLs for a checkpoint's
// content-addressed session blobs + the session-manifest.json. Sits
// alongside CreateCheckpoint (which handles the code blob URLs).
//
// userID tenancy-gates the operation: the worktree owning the
// checkpoint must belong to userID. The HTTP handler authorizes the
// caller (laptop vs sprite owner); gateway callers skip that step.
//
// Session blobs are content-addressed under the worktree, NOT the
// checkpoint — the same blob is shared across every checkpoint that
// references it. The manifest sidecar stays checkpoint-scoped.
func (s *Server) PresignSessionPuts(ctx context.Context, userID string, req SessionPresignRequest) (SessionPresignResult, error) {
	if req.CheckpointID == "" {
		return SessionPresignResult{}, fmt.Errorf("%w: checkpoint_id is required", ErrInvalidRequest)
	}

	_, wt, err := s.lookupCheckpointForUser(ctx, req.CheckpointID, userID)
	if err != nil {
		return SessionPresignResult{}, err
	}

	// Record the manifest digest on the checkpoint row so autosync can skip
	// the S3 manifest fetch when it already matches what the sprite imported.
	// Empty ⇒ skip (old clients / code-only pushes); no fallback default.
	if req.SessionsContentDigest != "" {
		if err := s.cfg.Store.UpdateCheckpointSessionsDigest(ctx, req.CheckpointID, req.SessionsContentDigest); err != nil {
			return SessionPresignResult{}, fmt.Errorf("sync: record session digest for %s: %w", req.CheckpointID, err)
		}
	}

	sessionURLs := make(map[string]string, len(req.Sessions))
	for _, ref := range req.Sessions {
		key, err := storage.KeyForSessionBlob(wt.UserID, wt.ID, ref.ExternalID, ref.ContentHash)
		if err != nil {
			return SessionPresignResult{}, fmt.Errorf("sync: key for session %s: %w", ref.ExternalID, err)
		}
		u, err := s.cfg.Storage.PresignPut(ctx, key, s.cfg.PresignTTL)
		if err != nil {
			return SessionPresignResult{}, fmt.Errorf("sync: presign session %s: %w", ref.ExternalID, err)
		}
		sessionURLs[ref.ExternalID] = u
	}

	manifestKey, err := storage.KeyFor(wt.UserID, wt.ID, req.CheckpointID, storage.BlobSessionManifest)
	if err != nil {
		return SessionPresignResult{}, fmt.Errorf("sync: key for session manifest: %w", err)
	}
	manifestURL, err := s.cfg.Storage.PresignPut(ctx, manifestKey, s.cfg.PresignTTL)
	if err != nil {
		return SessionPresignResult{}, fmt.Errorf("sync: presign session manifest: %w", err)
	}

	return SessionPresignResult{
		CheckpointID:          req.CheckpointID,
		SessionPutURLs:        sessionURLs,
		SessionManifestPutURL: manifestURL,
	}, nil
}

// SessionDownloadURLs is the result of Server.DownloadSessionURLs.
// SessionGetURLs is keyed by externalID. The manifest URL is for the
// session-manifest.json sidecar.
type SessionDownloadURLs struct {
	CheckpointID          string
	SessionGetURLs        map[string]string
	SessionManifestGetURL string
}

// DownloadSessionURLs mints presigned GET URLs for the
// session-manifest.json and the content-addressed per-session blobs of a
// checkpoint. The destination clank-host uses these to download the
// manifest, then per-session blobs, then call its own
// RegisterImportedSession.
//
// refs is the explicit set of session blobs to mint URLs for — derived
// from the SessionManifest after fetching it. Passing an empty slice
// mints only the manifest URL (the first hop, before the caller has read
// the manifest).
func (s *Server) DownloadSessionURLs(ctx context.Context, userID, checkpointID string, refs []checkpoint.SessionBlobRef) (SessionDownloadURLs, error) {
	if checkpointID == "" {
		return SessionDownloadURLs{}, fmt.Errorf("%w: checkpoint_id is required", ErrInvalidRequest)
	}

	ck, wt, err := s.lookupCheckpointForUser(ctx, checkpointID, userID)
	if err != nil {
		return SessionDownloadURLs{}, err
	}
	if ck.UploadedAt.IsZero() {
		return SessionDownloadURLs{}, fmt.Errorf("sync: checkpoint %s not yet uploaded", checkpointID)
	}

	manifestKey, err := storage.KeyFor(wt.UserID, wt.ID, ck.ID, storage.BlobSessionManifest)
	if err != nil {
		return SessionDownloadURLs{}, fmt.Errorf("sync: key for session manifest: %w", err)
	}
	manifestURL, err := s.cfg.Storage.PresignGet(ctx, manifestKey, s.cfg.PresignTTL)
	if err != nil {
		return SessionDownloadURLs{}, fmt.Errorf("sync: presign get session manifest: %w", err)
	}

	sessionURLs := make(map[string]string, len(refs))
	for _, ref := range refs {
		key, err := storage.KeyForSessionBlob(wt.UserID, wt.ID, ref.ExternalID, ref.ContentHash)
		if err != nil {
			return SessionDownloadURLs{}, fmt.Errorf("sync: key for session %s: %w", ref.ExternalID, err)
		}
		u, err := s.cfg.Storage.PresignGet(ctx, key, s.cfg.PresignTTL)
		if err != nil {
			return SessionDownloadURLs{}, fmt.Errorf("sync: presign get session %s: %w", ref.ExternalID, err)
		}
		sessionURLs[ref.ExternalID] = u
	}

	return SessionDownloadURLs{
		CheckpointID:          ck.ID,
		SessionGetURLs:        sessionURLs,
		SessionManifestGetURL: manifestURL,
	}, nil
}
