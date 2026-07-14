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
		Provider:   "flymachines",
		ExternalID: "sb-abc-123def456",
		Hostname:   "flymachines-123def456",
		Status:     store.HostStatusRunning,
		LastURL:    "https://example.preview.flymachines.app",
		LastToken:  "tok",
		AutoWake:   false,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	got, err := s.GetHostByUser(ctx, "local", "flymachines")
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
		Provider:   "flymachines",
		ExternalID: "sb-original",
		Hostname:   "flymachines-original",
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
		Provider:   "flymachines",
		ExternalID: "sb-new",
		Hostname:   "flymachines-new",
		Status:     store.HostStatusStopped,
		LastURL:    "https://new.preview",
		CreatedAt:  later, // ignored on update
		UpdatedAt:  later,
	}
	if err := s.UpsertHost(ctx, updated); err != nil {
		t.Fatalf("second UpsertHost: %v", err)
	}

	got, err := s.GetHostByUser(ctx, "local", "flymachines")
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
		Provider:   "flymachines",
		ExternalID: "sb-1",
		Hostname:   "flymachines-1",
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

	got, err := s.GetHostByUser(ctx, "local", "flymachines")
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

	_, err := s.GetHostByUser(ctx, "local", "flymachines")
	if !errors.Is(err, store.ErrHostNotFound) {
		t.Errorf("want ErrHostNotFound, got %v", err)
	}
}

func TestHosts_DifferentProviderCoexists(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	fm := store.Host{ID: "h-fm", UserID: "local", Provider: "flymachines", ExternalID: "sb-fm", Hostname: "flymachines-fm", Status: store.HostStatusRunning, CreatedAt: now, UpdatedAt: now}
	fly := store.Host{ID: "h-fly", UserID: "local", Provider: "flysprites", ExternalID: "sprite-x", Hostname: "flysprites-x", Status: store.HostStatusRunning, AutoWake: true, CreatedAt: now, UpdatedAt: now}
	if err := s.UpsertHost(ctx, fm); err != nil {
		t.Fatalf("upsert flymachines: %v", err)
	}
	if err := s.UpsertHost(ctx, fly); err != nil {
		t.Fatalf("upsert flysprites: %v", err)
	}

	gotFM, err := s.GetHostByUser(ctx, "local", "flymachines")
	if err != nil {
		t.Fatalf("get flymachines: %v", err)
	}
	if gotFM.ID != "h-fm" {
		t.Errorf("flymachines host id: got %q, want h-fm", gotFM.ID)
	}
	gotFly, err := s.GetHostByUser(ctx, "local", "flysprites")
	if err != nil {
		t.Fatalf("get flysprites: %v", err)
	}
	if gotFly.ID != "h-fly" {
		t.Errorf("flysprites host id: got %q, want h-fly", gotFly.ID)
	}
	if !gotFly.AutoWake {
		t.Error("flysprites host should have auto_wake=true")
	}
}

func TestHosts_Delete(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	h := store.Host{ID: "host-1", UserID: "local", Provider: "flymachines", ExternalID: "sb", Hostname: "flymachines-x", Status: store.HostStatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := s.DeleteHostByID(ctx, "host-1"); err != nil {
		t.Fatalf("delete by id: %v", err)
	}
	if _, err := s.GetHostByUser(ctx, "local", "flymachines"); !errors.Is(err, store.ErrHostNotFound) {
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
	if err := s.DeleteHostByUser(ctx, "local", "flymachines"); err != nil {
		t.Fatalf("delete by user: %v", err)
	}
	if _, err := s.GetHostByUser(ctx, "local", "flymachines"); !errors.Is(err, store.ErrHostNotFound) {
		t.Errorf("after delete-by-user: want ErrHostNotFound, got %v", err)
	}
}

func TestHosts_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	now := time.Now().Truncate(time.Second)

	{
		s := mustOpen(t, path)
		h := store.Host{ID: "h-persist", UserID: "local", Provider: "flymachines", ExternalID: "sb-persist", Hostname: "flymachines-persist", Status: store.HostStatusRunning, CreatedAt: now, UpdatedAt: now}
		if err := s.UpsertHost(context.Background(), h); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// Close happens via t.Cleanup in mustOpen.
	}

	// Reopen the same DB; the row should still be there.
	s := mustOpen(t, path)
	got, err := s.GetHostByUser(context.Background(), "local", "flymachines")
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
		{"missing-id", store.Host{UserID: "local", Provider: "flymachines"}},
		{"missing-user-id", store.Host{ID: "x", Provider: "flymachines"}},
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
