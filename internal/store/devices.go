package store

import (
	"context"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/store/sqlitedb"
)

// Device is the push-delivery address for one (user, app-install)
// pair. Mirrors the devices row but uses Go-friendly types. Platform
// is constrained to ios or android at the schema layer (CHECK).
type Device struct {
	UserID     string
	PushToken  string
	Platform   string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// Platform values accepted by the devices CHECK constraint. Kept here
// so callers don't sprinkle magic strings ("iOS"/"Ios"/"IOS") that
// would all bounce off the constraint.
const (
	DevicePlatformIOS     = "ios"
	DevicePlatformAndroid = "android"
)

// UpsertDevice inserts a new device row or refreshes last_seen_at on
// an existing one. created_at is preserved on update — only first-
// registration time is recorded. Callers fill UserID, PushToken,
// Platform; CreatedAt/LastSeenAt default to time.Now() when zero.
func (s *Store) UpsertDevice(ctx context.Context, d Device) error {
	if d.UserID == "" || d.PushToken == "" || d.Platform == "" {
		return fmt.Errorf("upsert device: user_id, push_token, platform are required")
	}
	now := time.Now()
	if d.LastSeenAt.IsZero() {
		d.LastSeenAt = now
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = d.LastSeenAt
	}
	return s.q.UpsertDevice(ctx, sqlitedb.UpsertDeviceParams{
		UserID:     d.UserID,
		PushToken:  d.PushToken,
		Platform:   d.Platform,
		CreatedAt:  d.CreatedAt,
		LastSeenAt: d.LastSeenAt,
	})
}

// ListDevicesByUser returns all push tokens registered for userID,
// most-recently-seen first. Empty slice (not error) when the user has
// no devices yet.
func (s *Store) ListDevicesByUser(ctx context.Context, userID string) ([]Device, error) {
	rows, err := s.q.ListDevicesByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list devices for user %s: %w", userID, err)
	}
	out := make([]Device, 0, len(rows))
	for _, r := range rows {
		out = append(out, Device{
			UserID:     r.UserID,
			PushToken:  r.PushToken,
			Platform:   r.Platform,
			CreatedAt:  r.CreatedAt,
			LastSeenAt: r.LastSeenAt,
		})
	}
	return out, nil
}

// DeleteDevice removes a single (user, push_token) device row. No-op
// when the row doesn't exist. Used on user-initiated logout.
func (s *Store) DeleteDevice(ctx context.Context, userID, pushToken string) error {
	return s.q.DeleteDevice(ctx, sqlitedb.DeleteDeviceParams{
		UserID:    userID,
		PushToken: pushToken,
	})
}

// DeleteDeviceByPushToken removes every row matching the given
// push_token, regardless of user. Used by the dispatcher when Expo
// returns DeviceNotRegistered — the token is dead system-wide, so
// purging all owners (typically only one) prevents future fan-outs
// from re-discovering the stale token.
func (s *Store) DeleteDeviceByPushToken(ctx context.Context, pushToken string) error {
	return s.q.DeleteDeviceByPushToken(ctx, pushToken)
}
