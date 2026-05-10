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
	OwnerKind  = clanksync.OwnerKind
)

const (
	OwnerKindLocal = clanksync.OwnerKindLocal
	OwnerKindRemote = clanksync.OwnerKindRemote
)

var (
	ErrWorktreeNotFound   = clanksync.ErrWorktreeNotFound
	ErrCheckpointNotFound = clanksync.ErrCheckpointNotFound
	ErrOwnerMismatch      = clanksync.ErrOwnerMismatch
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

// ListWorktreesByOwner returns all worktrees currently owned by the
// given (kind, ownerID). Used by the sprite at wake to discover what
// to apply (post-MVP P3+).
func (s *Store) ListWorktreesByOwner(ctx context.Context, kind OwnerKind, ownerID string) ([]Worktree, error) {
	rows, err := s.q.ListWorktreesByOwner(ctx, sqlitedb.ListWorktreesByOwnerParams{
		OwnerKind: string(kind),
		OwnerID:   ownerID,
	})
	if err != nil {
		return nil, fmt.Errorf("list worktrees (owner=%s/%s): %w", kind, ownerID, err)
	}
	out := make([]Worktree, len(rows))
	for i, r := range rows {
		out[i] = worktreeFromRow(r)
	}
	return out, nil
}

// InsertWorktree creates a new worktree row. ID, UserID, and
// DisplayName are required; CreatedAt / UpdatedAt default to now if
// zero. OwnerKind defaults to laptop.
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
	if w.OwnerKind == "" {
		w.OwnerKind = OwnerKindLocal
	}
	return s.q.InsertWorktree(ctx, sqlitedb.InsertWorktreeParams{
		ID:                     w.ID,
		UserID:                 w.UserID,
		DisplayName:            w.DisplayName,
		OwnerKind:              string(w.OwnerKind),
		OwnerID:                w.OwnerID,
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

// UpdateWorktreeOwner atomically transfers ownership iff the current
// (owner_kind, owner_id) tuple matches expectedKind/expectedOwnerID.
// Returns ErrOwnerMismatch on a failed guard (stale read or
// concurrent migration).
func (s *Store) UpdateWorktreeOwner(ctx context.Context, id string, expectedKind OwnerKind, expectedOwnerID string, newKind OwnerKind, newOwnerID string) error {
	rows, err := s.q.UpdateWorktreeOwner(ctx, sqlitedb.UpdateWorktreeOwnerParams{
		OwnerKind:   string(newKind),
		OwnerID:     newOwnerID,
		UpdatedAt:   time.Now(),
		ID:          id,
		OwnerKind_2: string(expectedKind),
		OwnerID_2:   expectedOwnerID,
	})
	if err != nil {
		return fmt.Errorf("update worktree owner (id=%s): %w", id, err)
	}
	if rows == 0 {
		return ErrOwnerMismatch
	}
	return nil
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
	if c.ID == "" || c.WorktreeID == "" || c.HeadCommit == "" || c.IndexTree == "" || c.WorktreeTree == "" || c.IncrementalCommit == "" {
		return fmt.Errorf("insert checkpoint: id, worktree_id, head_commit, index_tree, worktree_tree, incremental_commit are required")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	return s.q.InsertCheckpoint(ctx, sqlitedb.InsertCheckpointParams{
		ID:                c.ID,
		WorktreeID:        c.WorktreeID,
		HeadCommit:        c.HeadCommit,
		HeadRef:           c.HeadRef,
		IndexTree:         c.IndexTree,
		WorktreeTree:      c.WorktreeTree,
		IncrementalCommit: c.IncrementalCommit,
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

func worktreeFromRow(r sqlitedb.Worktree) Worktree {
	return Worktree{
		ID:                     r.ID,
		UserID:                 r.UserID,
		DisplayName:            r.DisplayName,
		OwnerKind:              OwnerKind(r.OwnerKind),
		OwnerID:                r.OwnerID,
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
		IncrementalCommit: r.IncrementalCommit,
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
