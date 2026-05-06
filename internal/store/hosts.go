package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/store/sqlitedb"
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
// One row per (user_id, provider) — see UNIQUE constraint in the schema.
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
	AuthToken   string
	AutoWake   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ErrHostNotFound is returned by GetHostByUser when no host matches.
// Callers should treat this as a non-error signal to provision.
var ErrHostNotFound = errors.New("host not found")

// GetHostByUser returns the single host for (userID, provider) or
// ErrHostNotFound. Other errors propagate as-is.
func (s *Store) GetHostByUser(ctx context.Context, userID, provider string) (Host, error) {
	row, err := s.q.GetHostByUser(ctx, sqlitedb.GetHostByUserParams{
		UserID:   userID,
		Provider: provider,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Host{}, ErrHostNotFound
	}
	if err != nil {
		return Host{}, fmt.Errorf("get host (user=%s provider=%s): %w", userID, provider, err)
	}
	return hostFromRow(row), nil
}

// GetHostByID returns a host by its internal ID, or ErrHostNotFound.
func (s *Store) GetHostByID(ctx context.Context, id string) (Host, error) {
	row, err := s.q.GetHostByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Host{}, ErrHostNotFound
	}
	if err != nil {
		return Host{}, fmt.Errorf("get host (id=%s): %w", id, err)
	}
	return hostFromRow(row), nil
}

// UpsertHost inserts or replaces by (user_id, provider). CreatedAt is
// preserved on update (UPSERT only updates the columns listed in the
// generated query). UpdatedAt is set to time.Now() if the caller
// provides a zero value.
func (s *Store) UpsertHost(ctx context.Context, h Host) error {
	if h.ID == "" || h.UserID == "" || h.Provider == "" {
		return fmt.Errorf("upsert host: id, user_id, provider are required")
	}
	if h.UpdatedAt.IsZero() {
		h.UpdatedAt = time.Now()
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = h.UpdatedAt
	}
	autoWake := int64(0)
	if h.AutoWake {
		autoWake = 1
	}
	return s.q.UpsertHost(ctx, sqlitedb.UpsertHostParams{
		ID:         h.ID,
		UserID:     h.UserID,
		Provider:   h.Provider,
		ExternalID: h.ExternalID,
		Hostname:   h.Hostname,
		Status:     string(h.Status),
		LastUrl:    h.LastURL,
		LastToken:  h.LastToken,
		AuthToken:   h.AuthToken,
		AutoWake:   autoWake,
		CreatedAt:  h.CreatedAt,
		UpdatedAt:  h.UpdatedAt,
	})
}

// DeleteHostByID removes a host row. No-op if the row doesn't exist.
func (s *Store) DeleteHostByID(ctx context.Context, id string) error {
	return s.q.DeleteHostByID(ctx, id)
}

// DeleteHostByUser removes the (user_id, provider) row, if any. Used
// when the provisioner detects out-of-band deletion at the provider
// (e.g. user nuked the sandbox via the Daytona dashboard) — clearing
// our row lets the next EnsureHost create a fresh one.
func (s *Store) DeleteHostByUser(ctx context.Context, userID, provider string) error {
	return s.q.DeleteHostByUser(ctx, sqlitedb.DeleteHostByUserParams{
		UserID:   userID,
		Provider: provider,
	})
}

func hostFromRow(r sqlitedb.Host) Host {
	return Host{
		ID:         r.ID,
		UserID:     r.UserID,
		Provider:   r.Provider,
		ExternalID: r.ExternalID,
		Hostname:   r.Hostname,
		Status:     HostStatus(r.Status),
		LastURL:    r.LastUrl,
		LastToken:  r.LastToken,
		AuthToken:   r.AuthToken,
		AutoWake:   r.AutoWake != 0,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
	}
}
