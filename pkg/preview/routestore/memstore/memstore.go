// Package memstore is an in-memory routestore.Store for tests.
//
// The cloud gateway uses a Postgres-backed adapter (cloud
// embedders supply it); a laptop daemon doesn't have a routestore
// at all because preview-URLs are a gateway-only feature with no
// laptop-side analog. So in production this package is unused.
//
// It exists so handler tests across packages (gateway webhook,
// gateway proxy, owner-facing tokens API) can wire a real
// routestore.Store without a Postgres dependency. It satisfies the
// same contract — soft-delete, expiry, idempotent upsert on the
// triple — so behaviors verified against it map 1:1 to the pgx
// store. Not a mock: every method runs the same logic the pgx
// adapter does, just over a map.
package memstore

import (
	"context"
	"sync"
	"time"

	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/tokens"
)

// Store is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	now    func() time.Time
	byTok  map[string]*routestore.Route
	byTrip map[tripleKey]string // (host_id, wid, service) → current token
}

// tripleKey is the upsert uniqueness key from the schema.
type tripleKey struct {
	HostID      string
	WorktreeID  string
	ServiceName string
}

// New returns an empty Store. Pass nil for the default now() (time.Now).
// Tests may inject a clock to control expiry without sleeping.
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		now:    now,
		byTok:  make(map[string]*routestore.Route),
		byTrip: make(map[tripleKey]string),
	}
}

var _ routestore.Store = (*Store)(nil)

func (s *Store) GetByToken(_ context.Context, token string) (routestore.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byTok[token]
	if !ok || !s.isLive(r) {
		return routestore.Route{}, routestore.ErrNotFound
	}
	return *r, nil
}

func (s *Store) Upsert(_ context.Context, r routestore.Route) (routestore.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tripleKey{r.HostID, r.WorktreeID, r.ServiceName}
	if existingTok, ok := s.byTrip[key]; ok {
		existing := s.byTok[existingTok]
		// Apply the same mutations the ON CONFLICT DO UPDATE clause does:
		// keep token + owner + created_at; refresh port + expiry + un-revoke.
		existing.InternalPort = r.InternalPort
		existing.ExpiresAt = r.ExpiresAt
		existing.RevokedAt = nil
		return *existing, nil
	}
	created := s.now()
	row := routestore.Route{
		Token:        r.Token,
		OwnerUserID:  r.OwnerUserID,
		HostID:       r.HostID,
		WorktreeID:   r.WorktreeID,
		ServiceName:  r.ServiceName,
		InternalPort: r.InternalPort,
		Visibility:   r.Visibility,
		CreatedAt:    created,
		ExpiresAt:    r.ExpiresAt,
	}
	s.byTok[r.Token] = &row
	s.byTrip[key] = r.Token
	return row, nil
}

func (s *Store) SetVisibility(_ context.Context, token string, v tokens.Visibility, expiresAt time.Time) (routestore.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byTok[token]
	if !ok || !s.isLive(r) {
		return routestore.Route{}, routestore.ErrNotFound
	}
	r.Visibility = v
	if !expiresAt.IsZero() {
		r.ExpiresAt = expiresAt
	}
	return *r, nil
}

func (s *Store) Revoke(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byTok[token]
	if !ok || !s.isLive(r) {
		return routestore.ErrNotFound
	}
	now := s.now()
	r.RevokedAt = &now
	return nil
}

func (s *Store) RevokeByService(_ context.Context, hostID, worktreeID, serviceName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.byTrip[tripleKey{hostID, worktreeID, serviceName}]
	if !ok {
		return nil // idempotent
	}
	r := s.byTok[tok]
	if r.RevokedAt != nil {
		return nil // already revoked
	}
	now := s.now()
	r.RevokedAt = &now
	return nil
}

func (s *Store) ListByOwner(_ context.Context, userID string) ([]routestore.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []routestore.Route
	for _, r := range s.byTok {
		if r.OwnerUserID == userID && s.isLive(r) {
			out = append(out, *r)
		}
	}
	// Stable ordering: newest first, matching the SQL ORDER BY created_at DESC.
	// Bubble sort is fine; tests will only have a handful of routes.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// isLive matches the SQL `revoked_at IS NULL AND expires_at > now()`.
// Caller holds s.mu.
func (s *Store) isLive(r *routestore.Route) bool {
	if r.RevokedAt != nil {
		return false
	}
	return r.ExpiresAt.After(s.now())
}
