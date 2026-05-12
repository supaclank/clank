package sync

import (
	"context"
	"errors"
	"fmt"
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

// ErrForbidden is returned by service-layer methods when the supplied
// userID doesn't own the requested resource (tenancy check failed).
// Caller-identity authorization (laptop vs sprite, etc.) is the HTTP
// handler's job and uses different error paths.
var ErrForbidden = errors.New("sync: forbidden")

// ErrBlobNotUploaded is returned by Server.CommitCheckpoint when one
// of the three required blobs hasn't shown up in object storage yet.
// HTTP handlers map this to 409 Conflict; gateway callers can retry
// after re-uploading.
var ErrBlobNotUploaded = errors.New("sync: blob not yet uploaded")

// ErrInvalidRequest is returned by service-layer methods when the
// supplied request fails validation (missing required fields, etc.).
// HTTP handlers map this to 400 Bad Request rather than letting plain
// validation errors flatten to 500.
var ErrInvalidRequest = errors.New("sync: invalid request")

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

// GetWorktree looks up a worktree by ID and verifies it belongs to
// userID (tenancy check). Service-layer counterpart to GET
// /v1/worktrees/{id}; gateway calls it directly during migration.
// Returns ErrWorktreeNotFound if missing, ErrForbidden if tenancy fails.
func (s *Server) GetWorktree(ctx context.Context, userID, worktreeID string) (Worktree, error) {
	wt, err := s.cfg.Store.GetWorktreeByID(ctx, worktreeID)
	if err != nil {
		return Worktree{}, err
	}
	if wt.UserID != userID {
		return Worktree{}, fmt.Errorf("%w: worktree %s", ErrForbidden, worktreeID)
	}
	return wt, nil
}

// ListWorktrees returns all worktrees belonging to userID. Used by the
// TUI to render ownership glyphs in the sidebar. Tenancy is enforced
// at the store level (ListWorktreesByUser).
func (s *Server) ListWorktrees(ctx context.Context, userID string) ([]Worktree, error) {
	return s.cfg.Store.ListWorktreesByUser(ctx, userID)
}

// TransferOwnership atomically transfers a worktree's owner. userID
// is the tenancy gate; toKind/toID/expectedOwnerID are forwarded to
// the store's optimistic-concurrency guard. Returns ErrOwnerMismatch
// on a lost-update race; callers retry from a fresh read.
func (s *Server) TransferOwnership(ctx context.Context, userID, worktreeID string, toKind OwnerKind, toID, expectedOwnerID string) (Worktree, error) {
	wt, err := s.cfg.Store.GetWorktreeByID(ctx, worktreeID)
	if errors.Is(err, ErrWorktreeNotFound) {
		return Worktree{}, err
	}
	if err != nil {
		return Worktree{}, fmt.Errorf("sync: get worktree: %w", err)
	}
	if wt.UserID != userID {
		return Worktree{}, fmt.Errorf("%w: worktree %s", ErrForbidden, worktreeID)
	}

	expected := expectedOwnerID
	if expected == "" {
		expected = wt.OwnerID
	}

	if err := s.cfg.Store.UpdateWorktreeOwner(ctx, worktreeID, wt.OwnerKind, expected, toKind, toID); err != nil {
		return Worktree{}, err
	}

	updated, err := s.cfg.Store.GetWorktreeByID(ctx, worktreeID)
	if err != nil {
		return Worktree{}, fmt.Errorf("sync: re-read after transfer: %w", err)
	}
	return updated, nil
}
