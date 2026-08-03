package memstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/supaclank/clank/pkg/preview/routestore"
	"github.com/supaclank/clank/pkg/preview/tokens"
)

func TestUpsert_NewAndConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(nil)
	expires := time.Now().Add(1 * time.Hour)

	// First insert returns the requested token.
	r1, err := s.Upsert(ctx, routestore.Route{
		Token:        "tok-first",
		OwnerUserID:  "u1",
		HostID:       "h1",
		WorktreeID:   "w1",
		ServiceName:  "default",
		InternalPort: 12000,
		Visibility:   tokens.VisibilityOwnerOnly,
		ExpiresAt:    expires,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if r1.Token != "tok-first" {
		t.Fatalf("first upsert returned %q, want %q", r1.Token, "tok-first")
	}

	// Second upsert for the same triple returns the original token,
	// but port + expires update. This is the sprite-restart contract.
	newExpires := expires.Add(2 * time.Hour)
	r2, err := s.Upsert(ctx, routestore.Route{
		Token:        "tok-second", // ignored
		OwnerUserID:  "u1",
		HostID:       "h1",
		WorktreeID:   "w1",
		ServiceName:  "default",
		InternalPort: 13000,
		Visibility:   tokens.VisibilityOwnerOnly,
		ExpiresAt:    newExpires,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if r2.Token != "tok-first" {
		t.Errorf("conflict upsert returned token %q, want existing %q", r2.Token, "tok-first")
	}
	if r2.InternalPort != 13000 {
		t.Errorf("port not updated: got %d, want %d", r2.InternalPort, 13000)
	}
	if !r2.ExpiresAt.Equal(newExpires) {
		t.Errorf("expires_at not refreshed: got %v, want %v", r2.ExpiresAt, newExpires)
	}
}

func TestUpsert_UnRevokesOnRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(nil)
	expires := time.Now().Add(1 * time.Hour)

	r, err := s.Upsert(ctx, routestore.Route{
		Token: "tok", OwnerUserID: "u", HostID: "h", WorktreeID: "w", ServiceName: "default",
		InternalPort: 1, Visibility: tokens.VisibilityOwnerOnly, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Revoke(ctx, r.Token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Now restart: same triple, fresh token candidate.
	r2, err := s.Upsert(ctx, routestore.Route{
		Token: "different", OwnerUserID: "u", HostID: "h", WorktreeID: "w", ServiceName: "default",
		InternalPort: 2, Visibility: tokens.VisibilityOwnerOnly, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("upsert after revoke: %v", err)
	}
	if r2.Token != "tok" {
		t.Errorf("restart should reuse original token, got %q", r2.Token)
	}
	if r2.RevokedAt != nil {
		t.Errorf("restart should un-revoke, got revoked_at=%v", *r2.RevokedAt)
	}
	// GetByToken now succeeds again.
	if _, err := s.GetByToken(ctx, "tok"); err != nil {
		t.Errorf("get after restart: %v", err)
	}
}

func TestGetByToken_TreatsRevokedAndExpiredAsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("revoked", func(t *testing.T) {
		t.Parallel()
		s := New(nil)
		_, err := s.Upsert(ctx, sampleRoute("rv", "h1"))
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := s.Revoke(ctx, "rv"); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		_, err = s.GetByToken(ctx, "rv")
		if !errors.Is(err, routestore.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		clock := now
		s := New(func() time.Time { return clock })
		r := sampleRoute("ex", "h2")
		r.ExpiresAt = now.Add(1 * time.Minute)
		if _, err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// Advance the clock past expiry.
		clock = now.Add(2 * time.Minute)
		_, err := s.GetByToken(ctx, "ex")
		if !errors.Is(err, routestore.ErrNotFound) {
			t.Errorf("expected ErrNotFound after expiry, got %v", err)
		}
	})
}

func TestSetVisibility_OnlyOnLiveRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(nil)
	if _, err := s.Upsert(ctx, sampleRoute("svt", "h")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	r, err := s.SetVisibility(ctx, "svt", tokens.VisibilityPublic, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("set visibility: %v", err)
	}
	if r.Visibility != tokens.VisibilityPublic {
		t.Errorf("got visibility %q, want %q", r.Visibility, tokens.VisibilityPublic)
	}
	// Revoke then try again — should be ErrNotFound.
	if err := s.Revoke(ctx, "svt"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.SetVisibility(ctx, "svt", tokens.VisibilityOwnerOnly, time.Now().Add(1*time.Hour)); !errors.Is(err, routestore.ErrNotFound) {
		t.Errorf("set on revoked: got %v, want ErrNotFound", err)
	}
}

func TestRevoke_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(nil)
	if _, err := s.Upsert(ctx, sampleRoute("rev", "h")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Revoke(ctx, "rev"); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	// Second revoke returns ErrNotFound (the row isn't live anymore).
	if err := s.Revoke(ctx, "rev"); !errors.Is(err, routestore.ErrNotFound) {
		t.Errorf("second revoke: got %v, want ErrNotFound", err)
	}
}

func TestRevokeByService_BestEffort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(nil)
	// No row exists — should be a no-op, not an error.
	if err := s.RevokeByService(ctx, "h-nope", "w-nope", "default"); err != nil {
		t.Errorf("revoke nonexistent: %v", err)
	}
	// Now create then revoke twice; second is also no-op.
	if _, err := s.Upsert(ctx, sampleRoute("rbst", "h")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.RevokeByService(ctx, "h", "w", "default"); err != nil {
		t.Fatalf("first revoke-by-service: %v", err)
	}
	if err := s.RevokeByService(ctx, "h", "w", "default"); err != nil {
		t.Errorf("second revoke-by-service: %v", err)
	}
}

func TestListByOwner_OnlyLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(nil)
	r1 := sampleRoute("a", "h1")
	r2 := sampleRoute("b", "h2")
	r3 := sampleRoute("c", "h3")
	r1.OwnerUserID, r2.OwnerUserID, r3.OwnerUserID = "alice", "alice", "bob"
	for _, r := range []routestore.Route{r1, r2, r3} {
		if _, err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	// Revoke one of alice's so it shouldn't appear.
	if err := s.Revoke(ctx, "a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out, err := s.ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 1 || out[0].Token != "b" {
		t.Errorf("got %d rows (%v), want only 'b'", len(out), tokensOf(out))
	}
}

func tokensOf(rs []routestore.Route) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Token
	}
	return out
}

// sampleRoute returns a route with sensible defaults for tests; caller
// can override fields before passing to Upsert.
func sampleRoute(token, hostID string) routestore.Route {
	return routestore.Route{
		Token:        token,
		OwnerUserID:  "u",
		HostID:       hostID,
		WorktreeID:   "w",
		ServiceName:  "default",
		InternalPort: 12000,
		Visibility:   tokens.VisibilityOwnerOnly,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
}
