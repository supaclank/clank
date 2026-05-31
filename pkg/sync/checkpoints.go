package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/acksell/clank/pkg/sync/storage"
)

// CreateCheckpointRequest is the typed input for Server.CreateCheckpoint.
// Caller identity (laptop vs sprite, ownership) is the HTTP handler's
// concern; gateway-direct callers fill CreatedBy with the appropriate
// stamp ("laptop:<device>" / "sprite:<host>").
type CreateCheckpointRequest struct {
	WorktreeID        string
	HeadCommit        string
	HeadRef           string
	IndexTree         string
	WorktreeTree      string
	UncommittedCommit string
	CreatedBy         string
}

// HeadBundleAction tells the client how to handle the head bundle for a
// checkpoint. The head bundle is content-addressed by HEAD SHA and shared
// across checkpoints, so the server can skip it entirely when it already
// holds that HEAD.
type HeadBundleAction string

const (
	// HeadBundleAlreadyStored: the server already has <user>/heads/<HEAD>.bundle.
	// The client uploads nothing for the head bundle.
	HeadBundleAlreadyStored HeadBundleAction = "already_stored"
	// HeadBundleUploadFull: upload a full head bundle (git bundle create f HEAD).
	HeadBundleUploadFull HeadBundleAction = "upload_full"
	// HeadBundleUploadIncremental: upload an incremental head bundle built
	// from HeadBundleBase (git bundle create f HEAD ^base). (Slice 2.)
	HeadBundleUploadIncremental HeadBundleAction = "upload_incremental"
)

// CreateCheckpointResult is the typed output of Server.CreateCheckpoint.
type CreateCheckpointResult struct {
	CheckpointID string
	// HeadBundleAction tells the client whether/how to upload the head
	// bundle; HeadBundlePutURL is set unless the action is already_stored;
	// HeadBundleBase is set only for upload_incremental.
	HeadBundleAction HeadBundleAction
	HeadBundlePutURL string
	HeadBundleBase   string
	UncommittedURL   string
	ManifestPutURL   string
	PresignTTL       time.Duration
	CreatedAt        time.Time
}

// CommitCheckpointResult is the typed output of Server.CommitCheckpoint.
type CommitCheckpointResult struct {
	CheckpointID string
	UploadedAt   time.Time
}

// CreateCheckpoint inserts a new checkpoint row, mints presigned PUT
// URLs for its three blobs, and returns both. This is the service-layer
// operation behind POST /v1/checkpoints; the HTTP handler decodes the
// body and authorizes the caller before calling here, the gateway
// calls it directly during a pull-back materialize.
//
// userID is the tenancy gate: the named worktree must belong to that
// user. Caller-identity authorization (laptop owns / sprite owns) is
// NOT performed here — it's the HTTP handler's job, because that's
// the only layer that has a verified Caller. Gateway callers are
// trusted in-process and skip the identity step.
func (s *Server) CreateCheckpoint(ctx context.Context, userID string, req CreateCheckpointRequest) (CreateCheckpointResult, error) {
	if req.WorktreeID == "" || req.HeadCommit == "" || req.IndexTree == "" ||
		req.WorktreeTree == "" || req.UncommittedCommit == "" {
		return CreateCheckpointResult{}, fmt.Errorf("%w: worktree_id, head_commit, index_tree, worktree_tree, uncommitted_commit are required", ErrInvalidRequest)
	}
	if req.CreatedBy == "" {
		return CreateCheckpointResult{}, fmt.Errorf("%w: created_by is required", ErrInvalidRequest)
	}

	wt, err := s.cfg.Store.GetWorktreeByID(ctx, req.WorktreeID)
	if errors.Is(err, ErrWorktreeNotFound) {
		return CreateCheckpointResult{}, err
	}
	if err != nil {
		return CreateCheckpointResult{}, fmt.Errorf("sync: get worktree: %w", err)
	}
	if wt.UserID != userID {
		return CreateCheckpointResult{}, fmt.Errorf("%w: worktree %s", ErrForbidden, req.WorktreeID)
	}

	now := time.Now().UTC()
	checkpointID := newULID()
	ck := Checkpoint{
		ID:                checkpointID,
		WorktreeID:        wt.ID,
		HeadCommit:        req.HeadCommit,
		HeadRef:           req.HeadRef,
		IndexTree:         req.IndexTree,
		WorktreeTree:      req.WorktreeTree,
		UncommittedCommit: req.UncommittedCommit,
		CreatedAt:         now,
		CreatedBy:         req.CreatedBy,
	}
	if err := s.cfg.Store.InsertCheckpoint(ctx, ck); err != nil {
		return CreateCheckpointResult{}, fmt.Errorf("sync: insert checkpoint: %w", err)
	}

	// Head bundle: content-addressed by HEAD SHA. If we already hold it,
	// tell the client to skip the upload entirely (the common idle case).
	headKey, err := storage.KeyForHead(wt.UserID, req.HeadCommit)
	if err != nil {
		return CreateCheckpointResult{}, fmt.Errorf("sync: head key: %w", err)
	}
	headExists, err := s.cfg.Storage.Exists(ctx, headKey)
	if err != nil {
		return CreateCheckpointResult{}, fmt.Errorf("sync: head exists check: %w", err)
	}
	headAction := HeadBundleAlreadyStored
	var headPutURL string
	if !headExists {
		headAction = HeadBundleUploadFull
		headPutURL, err = s.cfg.Storage.PresignPut(ctx, headKey, s.cfg.PresignTTL)
		if err != nil {
			return CreateCheckpointResult{}, fmt.Errorf("sync: presign head: %w", err)
		}
	}

	// Per-checkpoint blobs (uncommitted + manifest) always upload.
	urls, err := s.presignCheckpointPuts(ctx, wt.UserID, wt.ID, checkpointID)
	if err != nil {
		return CreateCheckpointResult{}, fmt.Errorf("sync: presign puts: %w", err)
	}

	return CreateCheckpointResult{
		CheckpointID:     checkpointID,
		HeadBundleAction: headAction,
		HeadBundlePutURL: headPutURL,
		UncommittedURL:   urls[storage.BlobUncommitted],
		ManifestPutURL:   urls[storage.BlobManifest],
		PresignTTL:       s.cfg.PresignTTL,
		CreatedAt:        now,
	}, nil
}

// CommitCheckpoint verifies all three blobs landed in storage and then
// atomically advances the worktree's latest_synced_checkpoint pointer.
// Service-layer operation behind POST /v1/checkpoints/{id}/commit.
//
// Same tenancy semantics as CreateCheckpoint: userID gates which
// tenant's checkpoints are addressable. Caller-identity authorization
// is the HTTP handler's job.
func (s *Server) CommitCheckpoint(ctx context.Context, userID, checkpointID string) (CommitCheckpointResult, error) {
	ck, wt, err := s.lookupCheckpointForUser(ctx, checkpointID, userID)
	if err != nil {
		return CommitCheckpointResult{}, err
	}

	// Head bundle is content-addressed by HEAD SHA (shared, separate key);
	// the per-checkpoint blobs live under the checkpoint prefix. Checked
	// in a fixed order so a missing-blob error is deterministic.
	headKey, err := storage.KeyForHead(wt.UserID, ck.HeadCommit)
	if err != nil {
		return CommitCheckpointResult{}, fmt.Errorf("sync: head key: %w", err)
	}
	uncommittedKey, err := storage.KeyFor(wt.UserID, wt.ID, ck.ID, storage.BlobUncommitted)
	if err != nil {
		return CommitCheckpointResult{}, fmt.Errorf("sync: build key: %w", err)
	}
	manifestKey, err := storage.KeyFor(wt.UserID, wt.ID, ck.ID, storage.BlobManifest)
	if err != nil {
		return CommitCheckpointResult{}, fmt.Errorf("sync: build key: %w", err)
	}
	for _, c := range []struct{ name, key string }{
		{"head bundle", headKey},
		{string(storage.BlobUncommitted), uncommittedKey},
		{string(storage.BlobManifest), manifestKey},
	} {
		exists, err := s.cfg.Storage.Exists(ctx, c.key)
		if err != nil {
			return CommitCheckpointResult{}, fmt.Errorf("sync: storage check %s: %w", c.name, err)
		}
		if !exists {
			return CommitCheckpointResult{}, fmt.Errorf("%w: %s", ErrBlobNotUploaded, c.name)
		}
	}

	now := time.Now().UTC()
	if err := s.cfg.Store.MarkCheckpointUploaded(ctx, ck.ID, now); err != nil {
		return CommitCheckpointResult{}, fmt.Errorf("sync: mark uploaded: %w", err)
	}
	if err := s.cfg.Store.UpdateWorktreePointer(ctx, wt.ID, ck.ID); err != nil {
		return CommitCheckpointResult{}, fmt.Errorf("sync: advance pointer: %w", err)
	}

	return CommitCheckpointResult{
		CheckpointID: ck.ID,
		UploadedAt:   now,
	}, nil
}

// DownloadCheckpointURLs mints presigned GET URLs for a committed
// checkpoint's three blobs. Same userID-tenancy gate as the other
// service methods. Returns an error if the checkpoint isn't yet
// uploaded.
func (s *Server) DownloadCheckpointURLs(ctx context.Context, userID, checkpointID string) (CheckpointDownloadURLs, error) {
	ck, wt, err := s.lookupCheckpointForUser(ctx, checkpointID, userID)
	if err != nil {
		return CheckpointDownloadURLs{}, err
	}
	if ck.UploadedAt.IsZero() {
		return CheckpointDownloadURLs{}, fmt.Errorf("sync: checkpoint %s not yet uploaded", checkpointID)
	}

	headKey, err := storage.KeyForHead(wt.UserID, ck.HeadCommit)
	if err != nil {
		return CheckpointDownloadURLs{}, fmt.Errorf("sync: head key: %w", err)
	}
	headGetURL, err := s.cfg.Storage.PresignGet(ctx, headKey, s.cfg.PresignTTL)
	if err != nil {
		return CheckpointDownloadURLs{}, fmt.Errorf("sync: presign head get: %w", err)
	}

	urls := make(map[storage.Blob]string, 2)
	for _, blob := range []storage.Blob{storage.BlobUncommitted, storage.BlobManifest} {
		key, err := storage.KeyFor(wt.UserID, wt.ID, ck.ID, blob)
		if err != nil {
			return CheckpointDownloadURLs{}, fmt.Errorf("sync: build key: %w", err)
		}
		u, err := s.cfg.Storage.PresignGet(ctx, key, s.cfg.PresignTTL)
		if err != nil {
			return CheckpointDownloadURLs{}, fmt.Errorf("sync: presign get: %w", err)
		}
		urls[blob] = u
	}

	return CheckpointDownloadURLs{
		CheckpointID:     ck.ID,
		HeadCommitGetURL: headGetURL,
		UncommittedURL:   urls[storage.BlobUncommitted],
		ManifestGetURL:   urls[storage.BlobManifest],
	}, nil
}

// CheckpointDownloadURLs is the result of Server.DownloadCheckpointURLs.
type CheckpointDownloadURLs struct {
	CheckpointID     string
	HeadCommitGetURL string
	UncommittedURL   string
	ManifestGetURL   string
}
