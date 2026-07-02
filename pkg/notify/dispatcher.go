package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/acksell/clank/internal/notifier"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
)

// HostLookup resolves a notifier bearer token to a host record. The
// dispatcher reads UserID off the result; HostID is exposed for logs
// and future per-host throttling.
//
// hoststore.HostStore satisfies this contract via GetHostByNotifierToken,
// so production wiring passes the existing daemon store; tests can
// substitute a fake.
type HostLookup interface {
	GetHostByNotifierToken(ctx context.Context, notifierToken string) (hoststore.Host, error)
}

// DeviceStore is the subset of internal/store.Store's devices API that
// the dispatcher needs. Lets tests inject a tiny in-memory fake without
// importing the SQLite store.
type DeviceStore interface {
	UpsertDevice(ctx context.Context, d Device) error
	ListDevicesByUser(ctx context.Context, userID string) ([]Device, error)
	DeleteDevice(ctx context.Context, userID, pushToken string) error
	DeleteDeviceByPushToken(ctx context.Context, pushToken string) error
}

// Device is the dispatcher's view of a registered push token. Mirrors
// internal/store.Device with the same field layout — passing through
// without conversion at the store boundary.
type Device struct {
	UserID     string
	PushToken  string
	Platform   string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// Pusher is the delivery contract. *Client satisfies it; tests
// substitute fakes that capture sent messages without HTTP.
type Pusher interface {
	Push(ctx context.Context, msgs []Message) ([]Ticket, error)
}

// Dispatcher receives notifier webhooks from hosts, resolves each to
// its owning user, and fans the notification out to every registered
// device. Construct with NewDispatcher.
type Dispatcher struct {
	hosts   HostLookup
	devices DeviceStore
	pusher  Pusher
	log     *log.Logger
}

// NewDispatcher wires up the dispatcher. A nil logger uses a stderr-
// prefixed default; the other dependencies are required.
func NewDispatcher(hosts HostLookup, devices DeviceStore, pusher Pusher, lg *log.Logger) *Dispatcher {
	if hosts == nil || devices == nil || pusher == nil {
		panic("notify.NewDispatcher: hosts, devices, pusher are required")
	}
	if lg == nil {
		lg = log.New(os.Stderr, "[notify] ", log.LstdFlags|log.Lmsgprefix)
	}
	return &Dispatcher{hosts: hosts, devices: devices, pusher: pusher, log: lg}
}

// Handle is bound at POST /webhooks/notifications. Flow:
//  1. Read Authorization: Bearer <token>.
//  2. Resolve token → host → user_id.
//  3. Decode the notifier.Notification body.
//  4. Load the user's registered devices.
//  5. Translate to Expo Messages and Push.
//  6. Purge any device row Expo flagged as DeviceNotRegistered.
//
// Status codes:
//
//	202 — accepted and dispatched (or accepted with zero devices).
//	400 — body decode failure.
//	401 — missing/unknown bearer token.
//	502 — Expo push failed (transport/whole-batch error).
func (d *Dispatcher) Handle(w http.ResponseWriter, r *http.Request) {
	token, err := auth.ExtractBearer(r)
	if err != nil {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
		return
	}
	host, err := d.hosts.GetHostByNotifierToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, hoststore.ErrHostNotFound) {
			http.Error(w, "unknown notifier token", http.StatusUnauthorized)
			return
		}
		d.log.Printf("host lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var n notifier.Notification
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, fmt.Sprintf("decode notification: %v", err), http.StatusBadRequest)
		return
	}

	devices, err := d.devices.ListDevicesByUser(r.Context(), host.UserID)
	if err != nil {
		d.log.Printf("list devices for user %s (host %s): %v", host.UserID, host.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(devices) == 0 {
		// No registered devices is a routine state (user signed out on
		// every device, or hasn't installed mobile yet). Don't 4xx —
		// the host did its job by posting; we silently accept.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	msgs := buildExpoMessages(n, devices)
	tickets, err := d.pusher.Push(r.Context(), msgs)
	if err != nil {
		d.log.Printf("expo push (user %s host %s kind %s): %v", host.UserID, host.ID, n.Kind, err)
		http.Error(w, "push failed", http.StatusBadGateway)
		return
	}
	d.purgeDeadTokens(r.Context(), msgs, tickets)
	w.WriteHeader(http.StatusAccepted)
}

// buildExpoMessages translates one Notification into one Expo Message
// per registered device. session_id is passed through as data so the
// mobile client can deep-link on tap. Priority is "high" for every
// classified kind — these are user-visible interrupts (idle finish,
// permission ask, error), not background sync.
func buildExpoMessages(n notifier.Notification, devices []Device) []Message {
	out := make([]Message, 0, len(devices))
	data := map[string]any{"session_id": n.SessionID, "kind": string(n.Kind)}
	for k, v := range n.Data {
		// Don't let payload keys overwrite the two clankd-owned ones.
		if k == "session_id" || k == "kind" {
			continue
		}
		data[k] = v
	}
	for _, dev := range devices {
		out = append(out, Message{
			To:       dev.PushToken,
			Title:    n.Title,
			Body:     n.Body,
			Data:     data,
			Priority: PriorityHigh,
			Sound:    "default",
		})
	}
	return out
}

func (d *Dispatcher) purgeDeadTokens(ctx context.Context, msgs []Message, tickets []Ticket) {
	if len(tickets) != len(msgs) {
		// Defense against a future Pusher impl that returns a mismatched
		// slice — without this we'd index out of range.
		return
	}
	for i, t := range tickets {
		// Dead tokens (Expo says so) and tokens from a foreign Expo
		// experience (client is pinned elsewhere — see WithExperienceID)
		// are both permanently undeliverable from this deployment.
		if !t.IsDeviceNotRegistered() && !t.IsMismatchedExperience() {
			continue
		}
		if err := d.devices.DeleteDeviceByPushToken(ctx, msgs[i].To); err != nil {
			d.log.Printf("purge stale device token=%s: %v", redactToken(msgs[i].To), err)
		}
	}
}

// maxDevicesPerUser caps registered devices per user. Real users have
// one phone, maybe two — the long tail of rows comes from reinstalls
// and dev-client rebuilds minting a fresh ExponentPushToken each time
// while the old row lingers (it's only purged if Expo happens to report
// it DeviceNotRegistered). On register we evict the least-recently-seen
// rows beyond the cap; a legitimately active device that gets evicted
// re-registers on its next app launch.
const maxDevicesPerUser = 5

// HandleRegister is bound at POST /devices behind the user-bearer
// auth middleware. Body: {"push_token": "...", "platform": "ios"|"android"}.
// Idempotent — re-registering the same token refreshes last_seen_at.
// Registration that pushes the user past maxDevicesPerUser evicts their
// stalest tokens.
func (d *Dispatcher) HandleRegister(w http.ResponseWriter, r *http.Request) {
	principal := auth.MustPrincipal(r.Context())
	var body struct {
		PushToken string `json:"push_token"`
		Platform  string `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	body.PushToken = strings.TrimSpace(body.PushToken)
	body.Platform = strings.TrimSpace(strings.ToLower(body.Platform))
	if body.PushToken == "" {
		http.Error(w, "push_token is required", http.StatusBadRequest)
		return
	}
	if body.Platform != "ios" && body.Platform != "android" {
		http.Error(w, "platform must be ios or android", http.StatusBadRequest)
		return
	}
	if err := d.devices.UpsertDevice(r.Context(), Device{
		UserID:    principal.UserID,
		PushToken: body.PushToken,
		Platform:  body.Platform,
	}); err != nil {
		d.log.Printf("upsert device (user %s): %v", principal.UserID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d.enforceDeviceCap(r.Context(), principal.UserID, body.PushToken)
	w.WriteHeader(http.StatusNoContent)
}

// enforceDeviceCap deletes the user's least-recently-seen device rows
// until at most maxDevicesPerUser remain. The row just registered is
// never evicted regardless of its stored last_seen_at. Best-effort:
// failures are logged, not surfaced — the registration itself already
// succeeded.
func (d *Dispatcher) enforceDeviceCap(ctx context.Context, userID, justRegistered string) {
	devs, err := d.devices.ListDevicesByUser(ctx, userID)
	if err != nil {
		d.log.Printf("device cap: list devices (user %s): %v", userID, err)
		return
	}
	if len(devs) <= maxDevicesPerUser {
		return
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].LastSeenAt.Before(devs[j].LastSeenAt) })
	excess := len(devs) - maxDevicesPerUser
	for _, dev := range devs {
		if excess == 0 {
			return
		}
		if dev.PushToken == justRegistered {
			continue
		}
		if err := d.devices.DeleteDevice(ctx, userID, dev.PushToken); err != nil {
			d.log.Printf("device cap: evict (user %s token=%s): %v", userID, redactToken(dev.PushToken), err)
			continue
		}
		d.log.Printf("device cap: evicted stale device (user %s token=%s, %d over cap)", userID, redactToken(dev.PushToken), excess)
		excess--
	}
}

// HandleDeregister is bound at DELETE /devices/{token}. Removes the
// (user, token) row. No-op when the row isn't there.
func (d *Dispatcher) HandleDeregister(w http.ResponseWriter, r *http.Request) {
	principal := auth.MustPrincipal(r.Context())
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	if err := d.devices.DeleteDevice(r.Context(), principal.UserID, token); err != nil {
		d.log.Printf("delete device (user %s token=%s): %v", principal.UserID, redactToken(token), err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// redactToken returns a short, log-safe fingerprint of an Expo push
// token. Push tokens identify a device-install pair across all of
// APNs/FCM — treat them like API keys in logs. Four chars at each end
// is enough to correlate two log lines belonging to the same token
// without revealing enough to replay one.
func redactToken(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "..." + s[len(s)-4:]
}
