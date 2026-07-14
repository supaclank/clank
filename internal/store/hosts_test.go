package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// --- CreateHostIfAbsent: the cross-instance provisioning claim ---

func TestHosts_CreateHostIfAbsent_InsertsThenReturnsWinner(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	first := store.Host{ID: "h1", UserID: "u1", Provider: "flymachines", ExternalID: "app-a", AuthToken: "tok-first"}
	got, created, err := s.CreateHostIfAbsent(ctx, first)
	if err != nil {
		t.Fatalf("first CreateHostIfAbsent: %v", err)
	}
	if !created || got.ID != "h1" || got.AuthToken != "tok-first" {
		t.Fatalf("first claim: created=%v got=%+v", created, got)
	}

	// A second claimer with DIFFERENT tokens must lose and read back the
	// winner's row — the exact token-divergence fix.
	second := store.Host{ID: "h2", UserID: "u1", Provider: "flymachines", ExternalID: "app-a", AuthToken: "tok-second"}
	got, created, err = s.CreateHostIfAbsent(ctx, second)
	if err != nil {
		t.Fatalf("second CreateHostIfAbsent: %v", err)
	}
	if created {
		t.Fatal("second claim reported created=true")
	}
	if got.ID != "h1" || got.AuthToken != "tok-first" {
		t.Fatalf("second claim did not return the winner: %+v", got)
	}
}

func TestHosts_CreateHostIfAbsent_ConcurrentSingleWinner(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	const n = 16
	var wg sync.WaitGroup
	rows := make([]store.Host, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := store.Host{
				ID:        fmt.Sprintf("h%d", i),
				UserID:    "u-conc",
				Provider:  "flymachines",
				AuthToken: fmt.Sprintf("tok-%d", i),
			}
			row, _, err := s.CreateHostIfAbsent(ctx, h)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			rows[i] = row
		}()
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if rows[i].ID != rows[0].ID || rows[i].AuthToken != rows[0].AuthToken {
			t.Fatalf("claim %d diverged: %+v vs %+v", i, rows[i], rows[0])
		}
	}
}

// --- CASProviderMeta: the resource-claim compare-and-set ---

func TestHosts_CASProviderMeta(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()
	if _, _, err := s.CreateHostIfAbsent(ctx, store.Host{ID: "h1", UserID: "u1", Provider: "flymachines"}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// absent → set wins
	won, current, err := s.CASProviderMeta(ctx, "h1", "volume_id", "", "vol_a")
	if err != nil || !won || current != "vol_a" {
		t.Fatalf("absent CAS: won=%v current=%q err=%v", won, current, err)
	}
	// present → if-absent CAS loses and reports the holder
	won, current, err = s.CASProviderMeta(ctx, "h1", "volume_id", "", "vol_b")
	if err != nil || won || current != "vol_a" {
		t.Fatalf("second if-absent CAS: won=%v current=%q err=%v", won, current, err)
	}
	// value survives a read
	row, err := s.GetHostByID(ctx, "h1")
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if row.ProviderMeta["volume_id"] != "vol_a" {
		t.Fatalf("meta after CAS = %v", row.ProviderMeta)
	}
	// stale-replacement: expected-old must match
	if won, _, err = s.CASProviderMeta(ctx, "h1", "volume_id", "vol_WRONG", "vol_c"); err != nil || won {
		t.Fatalf("wrong-old CAS won: won=%v err=%v", won, err)
	}
	if won, current, err = s.CASProviderMeta(ctx, "h1", "volume_id", "vol_a", "vol_c"); err != nil || !won || current != "vol_c" {
		t.Fatalf("correct-old CAS: won=%v current=%q err=%v", won, current, err)
	}
	// independent keys coexist
	if won, _, err = s.CASProviderMeta(ctx, "h1", "other_key", "", "x"); err != nil || !won {
		t.Fatalf("second key CAS: won=%v err=%v", won, err)
	}
	row, _ = s.GetHostByID(ctx, "h1")
	if row.ProviderMeta["volume_id"] != "vol_c" || row.ProviderMeta["other_key"] != "x" {
		t.Fatalf("meta after two keys = %v", row.ProviderMeta)
	}
	// missing host surfaces as an error, not a silent loss
	if _, _, err = s.CASProviderMeta(ctx, "missing", "k", "", "v"); err == nil {
		t.Fatal("CAS on missing host returned nil error")
	}
	// path-breaking keys rejected
	if _, _, err = s.CASProviderMeta(ctx, "h1", "bad.key", "", "v"); err == nil {
		t.Fatal("dotted key accepted")
	}
}

func TestHosts_CASProviderMeta_ConcurrentSingleWinner(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()
	if _, _, err := s.CreateHostIfAbsent(ctx, store.Host{ID: "h1", UserID: "u1", Provider: "flymachines"}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	winners := make([]bool, n)
	currents := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, current, err := s.CASProviderMeta(ctx, "h1", "volume_id", "", fmt.Sprintf("vol_%d", i))
			if err != nil {
				t.Errorf("CAS %d: %v", i, err)
				return
			}
			winners[i] = won
			currents[i] = current
		}()
	}
	wg.Wait()
	wonCount := 0
	for i := range n {
		if winners[i] {
			wonCount++
		}
		if currents[i] != currents[0] {
			t.Fatalf("CAS %d observed %q, others %q", i, currents[i], currents[0])
		}
	}
	if wonCount != 1 {
		t.Fatalf("CAS winners = %d, want exactly 1", wonCount)
	}
}

// TestHosts_UpsertPreservesProviderMeta pins the write-isolation rule:
// UpsertHost (the provisioner's status/URL writes) must never clobber
// the CAS-owned provider_meta.
func TestHosts_UpsertPreservesProviderMeta(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()
	h := store.Host{ID: "h1", UserID: "u1", Provider: "flymachines", AuthToken: "tok"}
	if _, _, err := s.CreateHostIfAbsent(ctx, h); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if won, _, err := s.CASProviderMeta(ctx, "h1", "volume_id", "", "vol_a"); err != nil || !won {
		t.Fatalf("seed CAS: won=%v err=%v", won, err)
	}

	h.Status = store.HostStatusRunning
	h.LastURL = "http://[fdaa::2]:8080"
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}
	row, err := s.GetHostByID(ctx, "h1")
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if row.ProviderMeta["volume_id"] != "vol_a" {
		t.Fatalf("UpsertHost clobbered provider_meta: %v", row.ProviderMeta)
	}
	if row.LastURL != h.LastURL || row.Status != store.HostStatusRunning {
		t.Fatalf("UpsertHost lost its own writes: %+v", row)
	}
}
