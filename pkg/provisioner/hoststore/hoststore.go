// Package hoststore defines the persistence contract used by cloud
// provisioners (flyio, daytona, …) for tracking the per-(userID, provider)
// host record. Splitting it out lets the laptop daemon back it with
// SQLite and lets external integrators (e.g. multi-tenant cloud control planes) back it with
// Postgres without modifying the provisioners themselves.
package hoststore

import (
	"context"
	"errors"
	"time"
)

// HostStatus is the lifecycle state of a persistent host as the
// provisioner most recently observed it. Values are strings rather than
// an enum so adding a new state (e.g. "archived") doesn't require a
// schema migration.
type HostStatus string

const (
	HostStatusRunning HostStatus = "running"
	HostStatusStopped HostStatus = "stopped"
	HostStatusError   HostStatus = "error"
)

// Host is the persistent record of a user's host across daemon restarts.
// One row per (UserID, Provider) — implementations enforce UNIQUE.
//
// LastURL/LastToken are cache hints, not the source of truth: a stale
// entry is expected after stop/resume and the provisioner refreshes
// them when /status fails.
//
// AuthToken is the clank-host bearer token, baked into the
// sandbox/sprite at create time. Stable across stop/resume — re-read
// on every EnsureHost so the local-side transport stays in sync.
type Host struct {
	ID         string
	UserID     string
	Provider   string
	ExternalID string
	Hostname   string
	Status     HostStatus
	LastURL    string
	LastToken  string
	AuthToken  string
	AutoWake   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ErrHostNotFound is returned by GetHostByUser/GetHostByID when no host
// matches. Callers should treat this as a non-error signal to provision.
var ErrHostNotFound = errors.New("host not found")

// HostStore is the persistence contract a provisioner depends on. The
// laptop daemon backs this with SQLite (clank/internal/store); external
// integrators back it with their own database (e.g. Postgres).
type HostStore interface {
	GetHostByUser(ctx context.Context, userID, provider string) (Host, error)
	GetHostByID(ctx context.Context, id string) (Host, error)
	UpsertHost(ctx context.Context, h Host) error
	DeleteHostByID(ctx context.Context, id string) error
	DeleteHostByUser(ctx context.Context, userID, provider string) error
}
