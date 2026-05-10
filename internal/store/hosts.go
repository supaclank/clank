package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/store/sqlitedb"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
)

// Type aliases — the canonical definitions live in
// pkg/provisioner/hoststore so cloud provisioners can take the
// HostStore interface without depending on internal/. Existing
// in-repo callers keep using store.Host etc. unchanged.
type (
	HostStatus = hoststore.HostStatus
	Host       = hoststore.Host
)

const (
	HostStatusRunning = hoststore.HostStatusRunning
	HostStatusStopped = hoststore.HostStatusStopped
	HostStatusError   = hoststore.HostStatusError
)

// ErrHostNotFound aliases hoststore.ErrHostNotFound so errors.Is keeps
// working across the package boundary.
var ErrHostNotFound = hoststore.ErrHostNotFound

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
