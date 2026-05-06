package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
)

func TestHosts_UpsertAndGet(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	h := store.Host{
		ID:         "host-1",
		UserID:     "local",
		Provider:   "daytona",
		ExternalID: "sb-abc-123def456",
		Hostname:   "daytona-123def456",
		Status:     store.HostStatusRunning,
		LastURL:    "https://example.preview.daytona.app",
		LastToken:  "tok",
		AutoWake:   false,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	got, err := s.GetHostByUser(ctx, "local", "daytona")
	if err != nil {
		t.Fatalf("GetHostByUser: %v", err)
	}
	if got.ID != h.ID || got.ExternalID != h.ExternalID || got.Hostname != h.Hostname {
		t.Errorf("round-trip mismatch:\nwant %+v\n got %+v", h, got)
	}
	if got.Status != store.HostStatusRunning {
		t.Errorf("status: got %q, want %q", got.Status, store.HostStatusRunning)
	}
	if got.LastURL != h.LastURL {
		t.Errorf("last_url: got %q, want %q", got.LastURL, h.LastURL)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("created_at: got %v, want %v", got.CreatedAt, now)
	}
}

func TestHosts_UpsertReplacesOnConflict(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	first := store.Host{
		ID:         "host-1",
		UserID:     "local",
		Provider:   "daytona",
		ExternalID: "sb-original",
		Hostname:   "daytona-original",
		Status:     store.HostStatusRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.UpsertHost(ctx, first); err != nil {
		t.Fatalf("first UpsertHost: %v", err)
	}

	// Same (user_id, provider), different ID and external_id — should
	// update the existing row's external_id/hostname/status fields per
	// the ON CONFLICT clause. ID stays as the original (PK is not in
	// the conflict-update list).
	later := now.Add(time.Hour)
	updated := store.Host{
		ID:         "host-1",
		UserID:     "local",
		Provider:   "daytona",
		ExternalID: "sb-new",
		Hostname:   "daytona-new",
		Status:     store.HostStatusStopped,
		LastURL:    "https://new.preview",
		CreatedAt:  later, // ignored on update
		UpdatedAt:  later,
	}
	if err := s.UpsertHost(ctx, updated); err != nil {
		t.Fatalf("second UpsertHost: %v", err)
	}

	got, err := s.GetHostByUser(ctx, "local", "daytona")
	if err != nil {
		t.Fatalf("GetHostByUser: %v", err)
	}
	if got.ExternalID != "sb-new" {
		t.Errorf("external_id should be updated, got %q", got.ExternalID)
	}
	if got.Status != store.HostStatusStopped {
		t.Errorf("status should be updated, got %q", got.Status)
	}
	if got.LastURL != "https://new.preview" {
		t.Errorf("last_url should be updated, got %q", got.LastURL)
	}
	// CreatedAt stays as the original — ON CONFLICT updates only the
	// columns the query explicitly sets.
	if !got.CreatedAt.Equal(now) {
		t.Errorf("created_at should be preserved across upsert, got %v want %v", got.CreatedAt, now)
	}
}

func TestHosts_UpsertConflictDifferentIDIsRejectedOrMerged(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	first := store.Host{
		ID:         "host-1",
		UserID:     "local",
		Provider:   "daytona",
		ExternalID: "sb-1",
		Hostname:   "daytona-1",
		Status:     store.HostStatusRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.UpsertHost(ctx, first); err != nil {
		t.Fatalf("first UpsertHost: %v", err)
	}

	// A second insert with a DIFFERENT id but the same (user_id, provider)
	// must NOT create two rows. Our UPSERT ON CONFLICT (user_id, provider)
	// causes the existing row to be updated; the new id is discarded.
	second := first
	second.ID = "host-2"
	second.ExternalID = "sb-2"
	if err := s.UpsertHost(ctx, second); err != nil {
		t.Fatalf("second UpsertHost: %v", err)
	}

	got, err := s.GetHostByUser(ctx, "local", "daytona")
	if err != nil {
		t.Fatalf("GetHostByUser: %v", err)
	}
	if got.ID != "host-1" {
		t.Errorf("primary key should be preserved across conflict-upsert; got id=%q want id=%q", got.ID, "host-1")
	}
	if got.ExternalID != "sb-2" {
		t.Errorf("external_id should reflect latest upsert; got %q want %q", got.ExternalID, "sb-2")
	}
}

func TestHosts_GetByUser_NotFound(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	_, err := s.GetHostByUser(ctx, "local", "daytona")
	if !errors.Is(err, store.ErrHostNotFound) {
		t.Errorf("want ErrHostNotFound, got %v", err)
	}
}

func TestHosts_DifferentProviderCoexists(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	dt := store.Host{ID: "h-dt", UserID: "local", Provider: "daytona", ExternalID: "sb-dt", Hostname: "daytona-dt", Status: store.HostStatusRunning, CreatedAt: now, UpdatedAt: now}
	fly := store.Host{ID: "h-fly", UserID: "local", Provider: "flyio", ExternalID: "sprite-x", Hostname: "flyio-x", Status: store.HostStatusRunning, AutoWake: true, CreatedAt: now, UpdatedAt: now}
	if err := s.UpsertHost(ctx, dt); err != nil {
		t.Fatalf("upsert daytona: %v", err)
	}
	if err := s.UpsertHost(ctx, fly); err != nil {
		t.Fatalf("upsert flyio: %v", err)
	}

	gotDT, err := s.GetHostByUser(ctx, "local", "daytona")
	if err != nil {
		t.Fatalf("get daytona: %v", err)
	}
	if gotDT.ID != "h-dt" {
		t.Errorf("daytona host id: got %q, want h-dt", gotDT.ID)
	}
	gotFly, err := s.GetHostByUser(ctx, "local", "flyio")
	if err != nil {
		t.Fatalf("get flyio: %v", err)
	}
	if gotFly.ID != "h-fly" {
		t.Errorf("flyio host id: got %q, want h-fly", gotFly.ID)
	}
	if !gotFly.AutoWake {
		t.Error("flyio host should have auto_wake=true")
	}
}

func TestHosts_Delete(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	h := store.Host{ID: "host-1", UserID: "local", Provider: "daytona", ExternalID: "sb", Hostname: "daytona-x", Status: store.HostStatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := s.DeleteHostByID(ctx, "host-1"); err != nil {
		t.Fatalf("delete by id: %v", err)
	}
	if _, err := s.GetHostByUser(ctx, "local", "daytona"); !errors.Is(err, store.ErrHostNotFound) {
		t.Errorf("after delete: want ErrHostNotFound, got %v", err)
	}

	// Delete-by-id on missing row is no-op (no error).
	if err := s.DeleteHostByID(ctx, "host-1"); err != nil {
		t.Errorf("delete missing: want nil, got %v", err)
	}

	// DeleteHostByUser variant.
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if err := s.DeleteHostByUser(ctx, "local", "daytona"); err != nil {
		t.Fatalf("delete by user: %v", err)
	}
	if _, err := s.GetHostByUser(ctx, "local", "daytona"); !errors.Is(err, store.ErrHostNotFound) {
		t.Errorf("after delete-by-user: want ErrHostNotFound, got %v", err)
	}
}

func TestHosts_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	now := time.Now().Truncate(time.Second)

	{
		s := mustOpen(t, path)
		h := store.Host{ID: "h-persist", UserID: "local", Provider: "daytona", ExternalID: "sb-persist", Hostname: "daytona-persist", Status: store.HostStatusRunning, CreatedAt: now, UpdatedAt: now}
		if err := s.UpsertHost(context.Background(), h); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// Close happens via t.Cleanup in mustOpen.
	}

	// Reopen the same DB; the row should still be there.
	s := mustOpen(t, path)
	got, err := s.GetHostByUser(context.Background(), "local", "daytona")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.ID != "h-persist" || got.ExternalID != "sb-persist" {
		t.Errorf("persistence mismatch: got %+v", got)
	}
}

func TestHosts_UpsertValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	cases := []struct {
		name string
		h    store.Host
	}{
		{"missing-id", store.Host{UserID: "local", Provider: "daytona"}},
		{"missing-user-id", store.Host{ID: "x", Provider: "daytona"}},
		{"missing-provider", store.Host{ID: "x", UserID: "local"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := s.UpsertHost(ctx, c.h); err == nil {
				t.Errorf("UpsertHost(%+v) returned nil error", c.h)
			}
		})
	}
}
