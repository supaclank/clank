package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore opens a fresh, isolated Store in a t.TempDir for the
// duration of the test. Mirrors the pattern in hosts_test.go.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDevices_UpsertAndList(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	want := []Device{
		{UserID: "alice", PushToken: "ExponentPushToken[111]", Platform: DevicePlatformIOS},
		{UserID: "alice", PushToken: "ExponentPushToken[222]", Platform: DevicePlatformAndroid},
		{UserID: "bob", PushToken: "ExponentPushToken[333]", Platform: DevicePlatformIOS},
	}
	for _, d := range want {
		if err := s.UpsertDevice(ctx, d); err != nil {
			t.Fatalf("UpsertDevice %+v: %v", d, err)
		}
	}

	got, err := s.ListDevicesByUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListDevicesByUser: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d alice devices, want 2", len(got))
	}
	for _, d := range got {
		if d.UserID != "alice" {
			t.Errorf("UserID = %q, want alice", d.UserID)
		}
		if d.CreatedAt.IsZero() || d.LastSeenAt.IsZero() {
			t.Error("CreatedAt and LastSeenAt must be set after upsert")
		}
	}

	bobs, err := s.ListDevicesByUser(ctx, "bob")
	if err != nil {
		t.Fatalf("ListDevicesByUser(bob): %v", err)
	}
	if len(bobs) != 1 {
		t.Errorf("got %d bob devices, want 1", len(bobs))
	}

	none, err := s.ListDevicesByUser(ctx, "ghost")
	if err != nil {
		t.Fatalf("ListDevicesByUser(ghost): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d ghost devices, want 0", len(none))
	}
}

func TestDevices_UpsertRefreshesLastSeen(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	first := Device{UserID: "alice", PushToken: "tok", Platform: DevicePlatformIOS}
	if err := s.UpsertDevice(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	rowsBefore, err := s.ListDevicesByUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListDevicesByUser (before): %v", err)
	}
	if len(rowsBefore) != 1 {
		t.Fatalf("got %d rows after first upsert, want 1", len(rowsBefore))
	}
	originalCreatedAt := rowsBefore[0].CreatedAt
	originalLastSeen := rowsBefore[0].LastSeenAt

	// Force a measurable gap so LastSeenAt advancement is observable.
	time.Sleep(15 * time.Millisecond)

	second := Device{UserID: "alice", PushToken: "tok", Platform: DevicePlatformIOS}
	if err := s.UpsertDevice(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	rowsAfter, err := s.ListDevicesByUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListDevicesByUser (after): %v", err)
	}
	if len(rowsAfter) != 1 {
		t.Fatalf("got %d rows after second upsert, want 1 (PK should collapse)", len(rowsAfter))
	}
	if !rowsAfter[0].CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("CreatedAt changed: %v → %v (must persist)", originalCreatedAt, rowsAfter[0].CreatedAt)
	}
	if !rowsAfter[0].LastSeenAt.After(originalLastSeen) {
		t.Errorf("LastSeenAt did not advance: %v → %v", originalLastSeen, rowsAfter[0].LastSeenAt)
	}
}

func TestDevices_Delete(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	d := Device{UserID: "alice", PushToken: "tok", Platform: DevicePlatformIOS}
	if err := s.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.DeleteDevice(ctx, "alice", "tok"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err := s.ListDevicesByUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListDevicesByUser: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows after delete, want 0", len(rows))
	}

	// Idempotent: deleting a missing row is fine.
	if err := s.DeleteDevice(ctx, "alice", "tok"); err != nil {
		t.Errorf("second delete should be no-op, got %v", err)
	}
}

func TestDevices_DeleteByPushTokenPurgesAllUsers(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Same push_token registered under two users (e.g. a shared
	// device a user signed out of and another signed into without
	// triggering the cleanup path).
	for _, user := range []string{"alice", "bob"} {
		if err := s.UpsertDevice(ctx, Device{UserID: user, PushToken: "shared", Platform: DevicePlatformIOS}); err != nil {
			t.Fatalf("upsert %s: %v", user, err)
		}
	}
	if err := s.DeleteDeviceByPushToken(ctx, "shared"); err != nil {
		t.Fatalf("DeleteDeviceByPushToken: %v", err)
	}
	for _, user := range []string{"alice", "bob"} {
		rows, err := s.ListDevicesByUser(ctx, user)
		if err != nil {
			t.Fatalf("ListDevicesByUser(%s): %v", user, err)
		}
		if len(rows) != 0 {
			t.Errorf("user %s: got %d rows after purge, want 0", user, len(rows))
		}
	}
}

func TestDevices_UpsertRejectsMissingFields(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	cases := []Device{
		{PushToken: "tok", Platform: DevicePlatformIOS}, // missing UserID
		{UserID: "alice", Platform: DevicePlatformIOS},  // missing PushToken
		{UserID: "alice", PushToken: "tok"},             // missing Platform
	}
	for _, d := range cases {
		if err := s.UpsertDevice(ctx, d); err == nil {
			t.Errorf("UpsertDevice(%+v) returned nil error; want validation failure", d)
		}
	}
}
