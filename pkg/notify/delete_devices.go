package notify

import (
	"context"
	"fmt"
)

// DeleteDevicesByUser removes every push-token row registered for userID.
// Used by account erasure. Idempotent — a user with no devices is a no-op.
// Aborts on the first delete error so the caller can retry; already-deleted
// rows are skipped on the retry (DeleteDevice is itself idempotent).
func (d *Dispatcher) DeleteDevicesByUser(ctx context.Context, userID string) error {
	devs, err := d.devices.ListDevicesByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("list devices for user %s: %w", userID, err)
	}
	for _, dev := range devs {
		if err := d.devices.DeleteDevice(ctx, userID, dev.PushToken); err != nil {
			return fmt.Errorf("delete device for user %s: %w", userID, err)
		}
	}
	return nil
}
