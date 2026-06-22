package notify

import (
	"context"
	"testing"
)

// TestDeleteDevicesByUser_RemovesOnlyTargetUser: erasing alice's devices
// leaves bob's intact (tenancy isolation), and a re-run is a no-op.
func TestDeleteDevicesByUser_RemovesOnlyTargetUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	devices := &fakeDeviceStore{}
	_ = devices.UpsertDevice(ctx, Device{UserID: "alice", PushToken: "a1", Platform: "ios"})
	_ = devices.UpsertDevice(ctx, Device{UserID: "alice", PushToken: "a2", Platform: "android"})
	_ = devices.UpsertDevice(ctx, Device{UserID: "bob", PushToken: "b1", Platform: "ios"})

	d := newDispatcher(t, newFakeHostLookup(), devices, &recordingPusher{})
	if err := d.DeleteDevicesByUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteDevicesByUser: %v", err)
	}

	remaining := devices.snapshot()
	if len(remaining) != 1 || remaining[0].UserID != "bob" {
		t.Fatalf("after delete, devices=%v, want only bob's", remaining)
	}

	// Idempotent: a second pass over an already-empty user changes nothing.
	if err := d.DeleteDevicesByUser(ctx, "alice"); err != nil {
		t.Fatalf("second DeleteDevicesByUser: %v", err)
	}
	if len(devices.snapshot()) != 1 {
		t.Fatal("second delete disturbed bob's device")
	}
}

// TestDeleteDevicesByUser_NoDevices: a user with no devices is a clean no-op.
func TestDeleteDevicesByUser_NoDevices(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t, newFakeHostLookup(), &fakeDeviceStore{}, &recordingPusher{})
	if err := d.DeleteDevicesByUser(context.Background(), "ghost"); err != nil {
		t.Fatalf("DeleteDevicesByUser on empty user: %v", err)
	}
}
