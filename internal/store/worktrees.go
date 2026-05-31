package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/store/sqlitedb"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// Type aliases — canonical definitions live in pkg/sync. Existing
// in-repo callers keep using store.Worktree etc. unchanged; the
// sync.Server consumes the SyncStore interface.
type (
	Worktree   = clanksync.Worktree
	Checkpoint = clanksync.Checkpoint
	HeadBundle = clanksync.HeadBundle
)

var (
	ErrWorktreeNotFound   = clanksync.ErrWorktreeNotFound
	ErrCheckpointNotFound = clanksync.ErrCheckpointNotFound
	ErrHeadBundleNotFound = clanksync.ErrHeadBundleNotFound
)

// GetWorktreeByID returns the worktree row or ErrWorktreeNotFound.
func (s *Store) GetWorktreeByID(ctx context.Context, id string) (Worktree, error) {
	row, err := s.q.GetWorktreeByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Worktree{}, ErrWorktreeNotFound
	}
	if err != nil {
		return Worktree{}, fmt.Errorf("get worktree (id=%s): %w", id, err)
	}
	return worktreeFromRow(row), nil
}

// ListWorktreesByUser returns all worktrees owned by a user, newest
// updated first.
func (s *Store) ListWorktreesByUser(ctx context.Context, userID string) ([]Worktree, error) {
	rows, err := s.q.ListWorktreesByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list worktrees (user=%s): %w", userID, err)
	}
	out := make([]Worktree, len(rows))
	for i, r := range rows {
		out[i] = worktreeFromRow(r)
	}
	return out, nil
}

// InsertWorktree creates a new worktree row. ID, UserID, and
// DisplayName are required; CreatedAt / UpdatedAt default to now if zero.
func (s *Store) InsertWorktree(ctx context.Context, w Worktree) error {
	if w.ID == "" || w.UserID == "" || w.DisplayName == "" {
		return fmt.Errorf("insert worktree: id, user_id, display_name are required")
	}
	if w.UpdatedAt.IsZero() {
		w.UpdatedAt = time.Now()
	}
	if w.CreatedAt.IsZero() {
		w.CreatedAt = w.UpdatedAt
	}
	return s.q.InsertWorktree(ctx, sqlitedb.InsertWorktreeParams{
		ID:                     w.ID,
		UserID:                 w.UserID,
		DisplayName:            w.DisplayName,
		OriginRepo:             w.OriginRepo,
		LatestSyncedCheckpoint: w.LatestSyncedCheckpoint,
		CreatedAt:              w.CreatedAt,
		UpdatedAt:              w.UpdatedAt,
	})
}

// UpdateWorktreePointer advances latest_synced_checkpoint after a
// checkpoint upload is committed.
func (s *Store) UpdateWorktreePointer(ctx context.Context, id, checkpointID string) error {
	return s.q.UpdateWorktreePointer(ctx, sqlitedb.UpdateWorktreePointerParams{
		LatestSyncedCheckpoint: checkpointID,
		UpdatedAt:              time.Now(),
		ID:                     id,
	})
}

// GetCheckpointByID returns a checkpoint row or ErrCheckpointNotFound.
func (s *Store) GetCheckpointByID(ctx context.Context, id string) (Checkpoint, error) {
	row, err := s.q.GetCheckpointByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, ErrCheckpointNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("get checkpoint (id=%s): %w", id, err)
	}
	return checkpointFromRow(row), nil
}

// ListCheckpointsByWorktree returns up to limit most-recent checkpoints
// for a worktree, newest first.
func (s *Store) ListCheckpointsByWorktree(ctx context.Context, worktreeID string, limit int) ([]Checkpoint, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.q.ListCheckpointsByWorktree(ctx, sqlitedb.ListCheckpointsByWorktreeParams{
		WorktreeID: worktreeID,
		Limit:      int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list checkpoints (worktree=%s): %w", worktreeID, err)
	}
	out := make([]Checkpoint, len(rows))
	for i, r := range rows {
		out[i] = checkpointFromRow(r)
	}
	return out, nil
}

// InsertCheckpoint records a checkpoint's metadata. UploadedAt remains
// NULL until MarkCheckpointUploaded is called after both bundles
// confirm.
func (s *Store) InsertCheckpoint(ctx context.Context, c Checkpoint) error {
	if c.ID == "" || c.WorktreeID == "" || c.HeadCommit == "" || c.IndexTree == "" || c.WorktreeTree == "" || c.UncommittedCommit == "" {
		return fmt.Errorf("insert checkpoint: id, worktree_id, head_commit, index_tree, worktree_tree, uncommitted_commit are required")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	return s.q.InsertCheckpoint(ctx, sqlitedb.InsertCheckpointParams{
		ID:           c.ID,
		WorktreeID:   c.WorktreeID,
		HeadCommit:   c.HeadCommit,
		HeadRef:      c.HeadRef,
		IndexTree:    c.IndexTree,
		WorktreeTree: c.WorktreeTree,
		// sqlc column keeps its legacy name `incremental_commit`; the
		// domain field is UncommittedCommit (renamed for clarity).
		IncrementalCommit: c.UncommittedCommit,
		CreatedAt:         c.CreatedAt,
		CreatedBy:         c.CreatedBy,
	})
}

// MarkCheckpointUploaded sets uploaded_at on a checkpoint after both
// bundles have confirmed upload.
func (s *Store) MarkCheckpointUploaded(ctx context.Context, id string, when time.Time) error {
	return s.q.MarkCheckpointUploaded(ctx, sqlitedb.MarkCheckpointUploadedParams{
		UploadedAt: sql.NullTime{Time: when, Valid: !when.IsZero()},
		ID:         id,
	})
}

// GetHeadBundle returns the head-bundle row for (userID, tipSHA) or
// ErrHeadBundleNotFound.
func (s *Store) GetHeadBundle(ctx context.Context, userID, tipSHA string) (HeadBundle, error) {
	row, err := s.q.GetHeadBundle(ctx, sqlitedb.GetHeadBundleParams{UserID: userID, TipSha: tipSHA})
	if errors.Is(err, sql.ErrNoRows) {
		return HeadBundle{}, ErrHeadBundleNotFound
	}
	if err != nil {
		return HeadBundle{}, fmt.Errorf("get head bundle (user=%s tip=%s): %w", userID, tipSHA, err)
	}
	return HeadBundle{
		UserID:    row.UserID,
		TipSHA:    row.TipSha,
		BaseSHA:   row.BaseSha,
		BlobKey:   row.BlobKey,
		CreatedAt: row.CreatedAt,
	}, nil
}

// InsertHeadBundle records a head-bundle row. Idempotent on
// (user_id, tip_sha) — INSERT OR IGNORE keeps the first stored bundle for
// a tip, so re-pushing an already-stored HEAD doesn't change its base.
func (s *Store) InsertHeadBundle(ctx context.Context, hb HeadBundle) error {
	if hb.UserID == "" || hb.TipSHA == "" || hb.BlobKey == "" {
		return fmt.Errorf("insert head bundle: user_id, tip_sha, blob_key are required")
	}
	if hb.CreatedAt.IsZero() {
		hb.CreatedAt = time.Now()
	}
	return s.q.InsertHeadBundle(ctx, sqlitedb.InsertHeadBundleParams{
		UserID:    hb.UserID,
		TipSha:    hb.TipSHA,
		BaseSha:   hb.BaseSHA,
		BlobKey:   hb.BlobKey,
		CreatedAt: hb.CreatedAt,
	})
}

func worktreeFromRow(r sqlitedb.Worktree) Worktree {
	return Worktree{
		ID:                     r.ID,
		UserID:                 r.UserID,
		DisplayName:            r.DisplayName,
		OriginRepo:             r.OriginRepo,
		LatestSyncedCheckpoint: r.LatestSyncedCheckpoint,
		CreatedAt:              r.CreatedAt,
		UpdatedAt:              r.UpdatedAt,
	}
}

func checkpointFromRow(r sqlitedb.Checkpoint) Checkpoint {
	c := Checkpoint{
		ID:                r.ID,
		WorktreeID:        r.WorktreeID,
		HeadCommit:        r.HeadCommit,
		HeadRef:           r.HeadRef,
		IndexTree:         r.IndexTree,
		WorktreeTree:      r.WorktreeTree,
		UncommittedCommit: r.IncrementalCommit, // legacy column name; see InsertCheckpoint
		CreatedAt:         r.CreatedAt,
		CreatedBy:         r.CreatedBy,
	}
	if r.UploadedAt.Valid {
		c.UploadedAt = r.UploadedAt.Time
	}
	return c
}

// compile-time check
var _ clanksync.SyncStore = (*Store)(nil)
