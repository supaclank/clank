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

// ErrHeadBundleNotFound is returned by SyncStore.GetHeadBundle when no
// head-bundle row exists for the (userID, tipSHA). Drives the
// full-vs-incremental decision on push and the chain walk on download.
var ErrHeadBundleNotFound = errors.New("sync: head bundle not found")

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

// Worktree is a per-user persistent unit of sync state. One row per
// logical working tree. Multiple worktrees can exist for the same user
// (and even the same repo, on different branches or worktrees).
type Worktree struct {
	ID          string
	UserID      string
	DisplayName string
	// OriginRepo identifies the repo this worktree was created from
	// (e.g. "acme/api" derived from the git remote, or the local dir
	// basename when no remote origin is configured). Used by clients
	// (mobile picker, TUI sidebar) to group worktrees by repo so users
	// don't see a flat list of unrelated ULIDs. Set once at registration;
	// never updated. May be "" for rows registered before this field
	// existed — clients group these under an "Unknown repo" bucket.
	OriginRepo             string
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
	UncommittedCommit string
	CreatedAt         time.Time
	CreatedBy         string
	UploadedAt        time.Time // zero until uploaded
}

// HeadBundle is the metadata for one content-addressed head bundle: the
// committed-history bundle ending at TipSHA, built from BaseSHA ("" = a
// full baseline with no prerequisite). Shared across a user's
// checkpoints/worktrees; the (BaseSHA → TipSHA) links form the chain.
type HeadBundle struct {
	UserID    string
	TipSHA    string
	BaseSHA   string // "" = full bundle (no prerequisite)
	BlobKey   string
	CreatedAt time.Time
}

// SyncStore is the persistence contract for worktrees + checkpoints +
// head bundles. Implementations MUST be safe for concurrent use.
type SyncStore interface {
	GetWorktreeByID(ctx context.Context, id string) (Worktree, error)
	ListWorktreesByUser(ctx context.Context, userID string) ([]Worktree, error)
	InsertWorktree(ctx context.Context, w Worktree) error
	UpdateWorktreePointer(ctx context.Context, id, checkpointID string) error

	GetCheckpointByID(ctx context.Context, id string) (Checkpoint, error)
	ListCheckpointsByWorktree(ctx context.Context, worktreeID string, limit int) ([]Checkpoint, error)
	InsertCheckpoint(ctx context.Context, c Checkpoint) error
	MarkCheckpointUploaded(ctx context.Context, id string, when time.Time) error

	// GetHeadBundle returns the head-bundle row for (userID, tipSHA), or
	// ErrHeadBundleNotFound. InsertHeadBundle is idempotent on
	// (userID, tipSHA) — the first stored bundle for a tip wins.
	GetHeadBundle(ctx context.Context, userID, tipSHA string) (HeadBundle, error)
	InsertHeadBundle(ctx context.Context, hb HeadBundle) error
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

// RegisterPrebuiltWorktree inserts a worktree row whose ID was assigned
// by the caller (rather than by the sync server). Used by the gateway
// after a mobile-initiated worktree create: the host generated the
// ULID inline (so the on-disk worktree-id file matches), and the
// gateway records the corresponding row here.
//
// Tenancy is enforced by the caller — by construction the gateway sets
// UserID from the authenticated principal before invoking this. Use
// the HTTP path POST /v1/worktrees (with handleRegisterWorktree) when
// the server should mint the ULID itself.
func (s *Server) RegisterPrebuiltWorktree(ctx context.Context, w Worktree) error {
	if w.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidRequest)
	}
	if w.UserID == "" {
		return fmt.Errorf("%w: user_id is required", ErrInvalidRequest)
	}
	return s.cfg.Store.InsertWorktree(ctx, w)
}
