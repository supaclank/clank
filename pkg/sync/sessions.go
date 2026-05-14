package sync

import (
	"context"
	"fmt"

	"github.com/acksell/clank/pkg/sync/storage"
)

// SessionPresignRequest is the input for Server.PresignSessionPuts. It
// rides on top of an existing checkpoint: the code checkpoint must
// already exist (typically just created by CreateCheckpoint) before
// sessions can be added.
type SessionPresignRequest struct {
	CheckpointID string
	SessionIDs   []string
}

// SessionPresignResult carries the per-session PUT URLs + the
// session-manifest.json PUT URL. SessionPutURLs is keyed by host ULID
// (the manifest's SessionEntry.SessionID).
type SessionPresignResult struct {
	CheckpointID         string
	SessionPutURLs       map[string]string
	SessionManifestPutURL string
}

// PresignSessionPuts mints presigned PUT URLs for a checkpoint's
// session blobs + the session-manifest.json. Sits alongside
// CreateCheckpoint (which handles the code blob URLs).
//
// userID tenancy-gates the operation: the worktree owning the
// checkpoint must belong to userID. The HTTP handler authorizes the
// caller (laptop vs sprite owner); gateway callers skip that step.
func (s *Server) PresignSessionPuts(ctx context.Context, userID string, req SessionPresignRequest) (SessionPresignResult, error) {
	if req.CheckpointID == "" {
		return SessionPresignResult{}, fmt.Errorf("%w: checkpoint_id is required", ErrInvalidRequest)
	}

	_, wt, err := s.lookupCheckpointForUser(ctx, req.CheckpointID, userID)
	if err != nil {
		return SessionPresignResult{}, err
	}

	sessionURLs := make(map[string]string, len(req.SessionIDs))
	for _, sid := range req.SessionIDs {
		key, err := storage.KeyForSession(wt.UserID, wt.ID, req.CheckpointID, sid)
		if err != nil {
			return SessionPresignResult{}, fmt.Errorf("sync: key for session %s: %w", sid, err)
		}
		u, err := s.cfg.Storage.PresignPut(ctx, key, s.cfg.PresignTTL)
		if err != nil {
			return SessionPresignResult{}, fmt.Errorf("sync: presign session %s: %w", sid, err)
		}
		sessionURLs[sid] = u
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
// SessionGetURLs is keyed by host ULID. The manifest URL is for the
// session-manifest.json sidecar.
type SessionDownloadURLs struct {
	CheckpointID           string
	SessionGetURLs         map[string]string
	SessionManifestGetURL  string
}

// DownloadSessionURLs mints presigned GET URLs for the
// session-manifest.json and per-session blobs of a checkpoint. The
// destination clank-host uses these to download the manifest, then
// per-session blobs, then call its own RegisterImportedSession.
//
// sessionIDs is the explicit set of sessions to mint URLs for —
// derived from the SessionManifest after fetching it. Passing an
// empty slice mints only the manifest URL (useful for the first hop
// where the caller hasn't read the manifest yet).
func (s *Server) DownloadSessionURLs(ctx context.Context, userID, checkpointID string, sessionIDs []string) (SessionDownloadURLs, error) {
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

	sessionURLs := make(map[string]string, len(sessionIDs))
	for _, sid := range sessionIDs {
		key, err := storage.KeyForSession(wt.UserID, wt.ID, ck.ID, sid)
		if err != nil {
			return SessionDownloadURLs{}, fmt.Errorf("sync: key for session %s: %w", sid, err)
		}
		u, err := s.cfg.Storage.PresignGet(ctx, key, s.cfg.PresignTTL)
		if err != nil {
			return SessionDownloadURLs{}, fmt.Errorf("sync: presign get session %s: %w", sid, err)
		}
		sessionURLs[sid] = u
	}

	return SessionDownloadURLs{
		CheckpointID:          ck.ID,
		SessionGetURLs:        sessionURLs,
		SessionManifestGetURL: manifestURL,
	}, nil
}
