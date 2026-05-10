package sync

import (
	"context"
	"errors"
	"time"
)

// ErrWorktreeNotFound is returned by SyncStore lookups when the
// requested worktree row doesn't exist.
var ErrWorktreeNotFound = errors.New("sync: worktree not found")

// ErrCheckpointNotFound is returned by SyncStore lookups when the
// requested checkpoint row doesn't exist.
var ErrCheckpointNotFound = errors.New("sync: checkpoint not found")

// ErrOwnerMismatch is returned by SyncStore.UpdateWorktreeOwner when
// the optimistic concurrency check fails — the expected current owner
// did not match what's in the row, indicating a concurrent migration
// or a stale read. Callers should retry from a fresh read.
var ErrOwnerMismatch = errors.New("sync: worktree owner mismatch (concurrent migration?)")

// OwnerKind enumerates which actor type owns a worktree's write
// authority. New values require schema-level coordination — never use
// raw string literals at call sites.
//
// "local" and "remote" are deliberately abstract: "local" covers the
// user's laptop today and any other on-user-device client (mobile,
// future) tomorrow; "remote" covers fly.io sprites today and any
// other off-user-device runtime (daytona, k8s, etc.) tomorrow. The
// concrete provisioner choice lives in the host store, not here.
type OwnerKind string

const (
	OwnerKindLocal  OwnerKind = "local"
	OwnerKindRemote OwnerKind = "remote"
)

// Worktree is a per-user persistent unit of sync ownership. One row
// per logical working tree. Multiple worktrees can exist for the same
// user (and even the same repo, on different branches or worktrees).
type Worktree struct {
	ID                     string
	UserID                 string
	DisplayName            string
	OwnerKind              OwnerKind
	OwnerID                string // device_id or host_id; "" if no owner has claimed yet
	LatestSyncedCheckpoint string // checkpoint ID; "" if no checkpoint pushed yet
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Checkpoint is the per-push manifest pointer. Bundle bytes live in
// object storage; this row is metadata only. UploadedAt is zero until
// /v1/checkpoints/<id>/commit confirms both bundles landed.
type Checkpoint struct {
	ID                string
	WorktreeID        string
	HeadCommit        string
	HeadRef           string
	IndexTree         string
	WorktreeTree      string
	IncrementalCommit string
	CreatedAt         time.Time
	CreatedBy         string
	UploadedAt        time.Time // zero until uploaded
}

// SyncStore is the persistence contract for worktrees + checkpoints.
// Implementations MUST be safe for concurrent use.
type SyncStore interface {
	GetWorktreeByID(ctx context.Context, id string) (Worktree, error)
	ListWorktreesByUser(ctx context.Context, userID string) ([]Worktree, error)
	ListWorktreesByOwner(ctx context.Context, kind OwnerKind, ownerID string) ([]Worktree, error)
	InsertWorktree(ctx context.Context, w Worktree) error
	UpdateWorktreePointer(ctx context.Context, id, checkpointID string) error

	// UpdateWorktreeOwner performs an atomic optimistic-concurrency
	// transfer: the row's owner is updated only if the full current
	// (owner_kind, owner_id) tuple matches the expected pair. Returns
	// ErrOwnerMismatch when the guard fails (stale read or
	// concurrent migration).
	UpdateWorktreeOwner(ctx context.Context, id string, expectedKind OwnerKind, expectedOwnerID string, newKind OwnerKind, newOwnerID string) error

	GetCheckpointByID(ctx context.Context, id string) (Checkpoint, error)
	ListCheckpointsByWorktree(ctx context.Context, worktreeID string, limit int) ([]Checkpoint, error)
	InsertCheckpoint(ctx context.Context, c Checkpoint) error
	MarkCheckpointUploaded(ctx context.Context, id string, when time.Time) error
}
