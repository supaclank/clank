// Package routestore is the contract for preview-route persistence.
//
// The gateway reads/writes routes through this contract; cloud
// embedders supply a pgx-backed implementation over a Postgres
// `preview_routes` table. The interface lives here so the gateway
// depends only on the contract, not on any provider-specific types —
// mirrors how provisioner/hoststore is structured.
//
// Lifecycle:
//
//	register webhook  →  Upsert        (idempotent on (host_id, worktree_id, service_name))
//	subdomain request →  GetByToken    (returns ErrNotFound for revoked or expired)
//	share endpoint    →  SetVisibility (also extends expires_at atomically)
//	revoke webhook    →  RevokeByService
//	DELETE endpoint   →  Revoke
//	owner GET list    →  ListByOwner   (live rows only)
package routestore

import (
	"context"
	"errors"
	"time"

	"github.com/acksell/clank/pkg/preview/tokens"
)

// ErrNotFound is returned by GetByToken / Revoke when no live row
// matches. "Live" means present, not revoked, and not expired —
// callers don't need to distinguish those at the contract level.
// Maps to HTTP 404 at the handler layer.
var ErrNotFound = errors.New("preview route not found")

// Route mirrors the preview_routes table row, in storage-neutral
// Go types. RevokedAt is a pointer so absence and "revoked at zero
// time" are distinguishable.
type Route struct {
	Token        string
	OwnerUserID  string
	HostID       string
	WorktreeID   string
	ServiceName  string
	InternalPort int
	Visibility   tokens.Visibility
	CreatedAt    time.Time
	ExpiresAt    time.Time
	RevokedAt    *time.Time
}

// Store is the persistence contract.
//
// Implementations MUST treat (host_id, worktree_id, service_name) as
// the upsert key — sprite restart re-registers the same triple and
// must get the existing token back so mobile's stored URL doesn't
// churn. The Upsert SQL pattern is INSERT ... ON CONFLICT DO UPDATE
// (everything except token) RETURNING *.
type Store interface {
	// GetByToken returns the live row for token, or ErrNotFound when
	// the row is missing, expired, or revoked. The proxy uses this to
	// resolve every inbound request, so implementations should add an
	// in-memory cache (with revoke invalidation via webhook).
	GetByToken(ctx context.Context, token string) (Route, error)

	// Upsert inserts r if no row exists for its (HostID, WorktreeID,
	// ServiceName) triple, else updates internal_port + expires_at on
	// the existing row and clears revoked_at. Returns the row as it
	// exists after the operation — its Token is the canonical one
	// (which equals r.Token only when no conflict occurred).
	//
	// r.Token MUST be a freshly minted token (the caller doesn't know
	// in advance whether the row exists). r.OwnerUserID, HostID,
	// WorktreeID, ServiceName, InternalPort, Visibility, ExpiresAt
	// must all be set; CreatedAt is ignored (server-set).
	Upsert(ctx context.Context, r Route) (Route, error)

	// SetVisibility flips visibility on the named token and extends
	// expires_at to the supplied time (or leaves it untouched if zero).
	// Returns the post-update row. ErrNotFound when no live row matches.
	SetVisibility(ctx context.Context, token string, v tokens.Visibility, expiresAt time.Time) (Route, error)

	// Revoke marks the named token revoked. ErrNotFound when no live
	// row matches. Idempotent — revoking an already-revoked row also
	// returns ErrNotFound (the row isn't live).
	Revoke(ctx context.Context, token string) error

	// RevokeByService is the sprite-driven revoke. The sprite knows
	// the triple but not the token (it discarded the token after
	// returning it to mobile). Idempotent — returns nil when no row
	// matches, because the sprite's preview/stop is best-effort.
	RevokeByService(ctx context.Context, hostID, worktreeID, serviceName string) error

	// ListByOwner returns every live route owned by userID, ordered
	// by created_at DESC. Used by the owner-facing GET /v1/preview/tokens.
	ListByOwner(ctx context.Context, userID string) ([]Route, error)
}
