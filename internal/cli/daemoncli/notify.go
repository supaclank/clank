package daemoncli

import (
	"context"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/notify"
)

// notifyDeviceAdapter bridges *store.Store (which speaks store.Device)
// to notify.DeviceStore (which speaks notify.Device). The two structs
// have identical field shapes — the adapter exists only because the
// dispatcher lives in pkg/notify and can't import internal/store.
//
// Keeping the conversion at the daemoncli wiring layer means the
// dispatcher tests can still talk to a tiny in-memory fake without
// pulling in SQLite.
type notifyDeviceAdapter struct{ s *store.Store }

func (a notifyDeviceAdapter) UpsertDevice(ctx context.Context, d notify.Device) error {
	return a.s.UpsertDevice(ctx, store.Device{
		UserID:     d.UserID,
		PushToken:  d.PushToken,
		Platform:   d.Platform,
		CreatedAt:  d.CreatedAt,
		LastSeenAt: d.LastSeenAt,
	})
}

func (a notifyDeviceAdapter) ListDevicesByUser(ctx context.Context, userID string) ([]notify.Device, error) {
	rows, err := a.s.ListDevicesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]notify.Device, 0, len(rows))
	for _, r := range rows {
		out = append(out, notify.Device{
			UserID:     r.UserID,
			PushToken:  r.PushToken,
			Platform:   r.Platform,
			CreatedAt:  r.CreatedAt,
			LastSeenAt: r.LastSeenAt,
		})
	}
	return out, nil
}

func (a notifyDeviceAdapter) DeleteDevice(ctx context.Context, userID, pushToken string) error {
	return a.s.DeleteDevice(ctx, userID, pushToken)
}

func (a notifyDeviceAdapter) DeleteDeviceByPushToken(ctx context.Context, pushToken string) error {
	return a.s.DeleteDeviceByPushToken(ctx, pushToken)
}
