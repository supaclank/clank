package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	return hostFromRow(row)
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
	return hostFromRow(row)
}

// GetHostByNotifierToken returns the host whose notifier_token matches
// the given plaintext bearer. Returns ErrHostNotFound on miss or when
// notifierToken is empty (empty matches every legacy row, which is
// exactly the wrong thing). Fast-fails before hitting the DB to avoid
// that footgun.
func (s *Store) GetHostByNotifierToken(ctx context.Context, notifierToken string) (Host, error) {
	if notifierToken == "" {
		return Host{}, ErrHostNotFound
	}
	row, err := s.q.GetHostByNotifierToken(ctx, notifierToken)
	if errors.Is(err, sql.ErrNoRows) {
		return Host{}, ErrHostNotFound
	}
	if err != nil {
		return Host{}, fmt.Errorf("get host by notifier_token: %w", err)
	}
	return hostFromRow(row)
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
		ID:            h.ID,
		UserID:        h.UserID,
		Provider:      h.Provider,
		ExternalID:    h.ExternalID,
		Hostname:      h.Hostname,
		Status:        string(h.Status),
		LastUrl:       h.LastURL,
		LastToken:     h.LastToken,
		AuthToken:     h.AuthToken,
		NotifierToken: h.NotifierToken,
		AutoWake:      autoWake,
		CreatedAt:     h.CreatedAt,
		UpdatedAt:     h.UpdatedAt,
	})
}

// CreateHostIfAbsent implements hoststore.HostStore: atomically insert
// h keyed on UNIQUE(user_id, provider), or return the row that beat us.
// The cross-instance provisioning claim — see the interface doc.
func (s *Store) CreateHostIfAbsent(ctx context.Context, h Host) (Host, bool, error) {
	if h.ID == "" || h.UserID == "" || h.Provider == "" {
		return Host{}, false, fmt.Errorf("create host if absent: id, user_id, provider are required")
	}
	if h.UpdatedAt.IsZero() {
		h.UpdatedAt = time.Now().UTC()
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = h.UpdatedAt
	}
	autoWake := int64(0)
	if h.AutoWake {
		autoWake = 1
	}
	n, err := s.q.InsertHostIfAbsent(ctx, sqlitedb.InsertHostIfAbsentParams{
		ID:            h.ID,
		UserID:        h.UserID,
		Provider:      h.Provider,
		ExternalID:    h.ExternalID,
		Hostname:      h.Hostname,
		Status:        string(h.Status),
		LastUrl:       h.LastURL,
		LastToken:     h.LastToken,
		AuthToken:     h.AuthToken,
		NotifierToken: h.NotifierToken,
		AutoWake:      autoWake,
		CreatedAt:     h.CreatedAt,
		UpdatedAt:     h.UpdatedAt,
	})
	if err != nil {
		return Host{}, false, fmt.Errorf("insert host if absent (user=%s provider=%s): %w", h.UserID, h.Provider, err)
	}
	if n == 1 {
		return h, true, nil
	}
	existing, err := s.GetHostByUser(ctx, h.UserID, h.Provider)
	if err != nil {
		// Insert lost to a row that vanished before our read-back (a
		// concurrent destroy) — surface it; the caller's next attempt
		// re-claims cleanly.
		return Host{}, false, fmt.Errorf("read back winning host row (user=%s provider=%s): %w", h.UserID, h.Provider, err)
	}
	return existing, false, nil
}

// CASProviderMeta implements hoststore.HostStore. Hand-written SQL:
// sqlc's SQLite grammar rejects bound parameters inside
// json_set/json_extract. key must be a plain identifier (no dots or
// quotes) — it becomes a JSON path segment.
func (s *Store) CASProviderMeta(ctx context.Context, hostID, key, oldValue, newValue string) (bool, string, error) {
	if hostID == "" || key == "" || newValue == "" {
		return false, "", fmt.Errorf("cas provider meta: hostID, key, newValue are required")
	}
	for _, r := range key {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum && r != '_' {
			return false, "", fmt.Errorf("cas provider meta: key %q must be a plain alphanumeric identifier", key)
		}
	}
	path := "$." + key
	res, err := s.db.ExecContext(ctx, `
		UPDATE hosts
		SET provider_meta = json_set(provider_meta, ?, ?),
		    updated_at    = ?
		WHERE id = ? AND COALESCE(json_extract(provider_meta, ?), '') = ?`,
		path, newValue, time.Now().UTC(), hostID, path, oldValue)
	if err != nil {
		return false, "", fmt.Errorf("cas provider_meta[%s] on host %s: %w", key, hostID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, "", fmt.Errorf("cas provider_meta[%s] rows affected: %w", key, err)
	}
	if n == 1 {
		return true, newValue, nil
	}
	row, err := s.GetHostByID(ctx, hostID)
	if err != nil {
		return false, "", fmt.Errorf("read back provider_meta[%s] on host %s: %w", key, hostID, err)
	}
	return false, row.ProviderMeta[key], nil
}

// DeleteHostByID removes a host row. No-op if the row doesn't exist.
func (s *Store) DeleteHostByID(ctx context.Context, id string) error {
	return s.q.DeleteHostByID(ctx, id)
}

// DeleteHostByUser removes the (user_id, provider) row, if any. Used
// when the provisioner detects out-of-band deletion at the provider
// (e.g. user nuked the sandbox via the provider dashboard) — clearing
// our row lets the next EnsureHost create a fresh one.
func (s *Store) DeleteHostByUser(ctx context.Context, userID, provider string) error {
	return s.q.DeleteHostByUser(ctx, sqlitedb.DeleteHostByUserParams{
		UserID:   userID,
		Provider: provider,
	})
}

func hostFromRow(r sqlitedb.Host) (Host, error) {
	var meta map[string]string
	if r.ProviderMeta != "" && r.ProviderMeta != "{}" {
		if err := json.Unmarshal([]byte(r.ProviderMeta), &meta); err != nil {
			return Host{}, fmt.Errorf("host %s: malformed provider_meta: %w", r.ID, err)
		}
	}
	return Host{
		ID:            r.ID,
		UserID:        r.UserID,
		Provider:      r.Provider,
		ExternalID:    r.ExternalID,
		Hostname:      r.Hostname,
		Status:        HostStatus(r.Status),
		LastURL:       r.LastUrl,
		LastToken:     r.LastToken,
		AuthToken:     r.AuthToken,
		NotifierToken: r.NotifierToken,
		AutoWake:      r.AutoWake != 0,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		ProviderMeta:  meta,
	}, nil
}
