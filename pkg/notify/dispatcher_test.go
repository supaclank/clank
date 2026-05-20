package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/notifier"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
)

// fakeHostLookup is a real HostLookup implementation backed by an
// in-memory map. Not a mock — the dispatcher talks to it through the
// real interface.
type fakeHostLookup struct {
	mu    sync.Mutex
	hosts map[string]hoststore.Host
}

func newFakeHostLookup(hosts ...hoststore.Host) *fakeHostLookup {
	m := map[string]hoststore.Host{}
	for _, h := range hosts {
		m[h.NotifierToken] = h
	}
	return &fakeHostLookup{hosts: m}
}

func (f *fakeHostLookup) GetHostByNotifierToken(_ context.Context, token string) (hoststore.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.hosts[token]
	if !ok {
		return hoststore.Host{}, hoststore.ErrHostNotFound
	}
	return h, nil
}

// fakeDeviceStore is a real DeviceStore backed by a slice. Captures
// upsert/delete calls so tests can assert on them.
type fakeDeviceStore struct {
	mu      sync.Mutex
	devices []Device
}

func (f *fakeDeviceStore) UpsertDevice(_ context.Context, d Device) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, ex := range f.devices {
		if ex.UserID == d.UserID && ex.PushToken == d.PushToken {
			d.CreatedAt = ex.CreatedAt
			if d.LastSeenAt.IsZero() {
				d.LastSeenAt = time.Now()
			}
			f.devices[i] = d
			return nil
		}
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	if d.LastSeenAt.IsZero() {
		d.LastSeenAt = d.CreatedAt
	}
	f.devices = append(f.devices, d)
	return nil
}

func (f *fakeDeviceStore) ListDevicesByUser(_ context.Context, userID string) ([]Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Device
	for _, d := range f.devices {
		if d.UserID == userID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeDeviceStore) DeleteDevice(_ context.Context, userID, pushToken string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.devices[:0]
	for _, d := range f.devices {
		if d.UserID == userID && d.PushToken == pushToken {
			continue
		}
		kept = append(kept, d)
	}
	f.devices = kept
	return nil
}

func (f *fakeDeviceStore) DeleteDeviceByPushToken(_ context.Context, pushToken string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.devices[:0]
	for _, d := range f.devices {
		if d.PushToken == pushToken {
			continue
		}
		kept = append(kept, d)
	}
	f.devices = kept
	return nil
}

func (f *fakeDeviceStore) snapshot() []Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Device, len(f.devices))
	copy(out, f.devices)
	return out
}

// recordingPusher is a Pusher fixture that captures every Push call
// and returns caller-controlled tickets.
type recordingPusher struct {
	mu        sync.Mutex
	sent      []Message
	ticketFor func(m Message) Ticket
	pushErr   error
}

func (p *recordingPusher) Push(_ context.Context, msgs []Message) ([]Ticket, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pushErr != nil {
		return nil, p.pushErr
	}
	p.sent = append(p.sent, msgs...)
	tickets := make([]Ticket, len(msgs))
	for i, m := range msgs {
		if p.ticketFor != nil {
			tickets[i] = p.ticketFor(m)
		} else {
			tickets[i] = Ticket{Status: "ok"}
		}
	}
	return tickets, nil
}

func (p *recordingPusher) sentSnapshot() []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Message, len(p.sent))
	copy(out, p.sent)
	return out
}

func newDispatcher(t *testing.T, hosts *fakeHostLookup, devices *fakeDeviceStore, pusher *recordingPusher) *Dispatcher {
	t.Helper()
	return NewDispatcher(hosts, devices, pusher, nil)
}

func postNotification(t *testing.T, d *Dispatcher, token string, n notifier.Notification) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/webhooks/notifications", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	d.Handle(w, r)
	return w
}

func TestDispatcher_Handle_HappyPath(t *testing.T) {
	t.Parallel()
	hosts := newFakeHostLookup(hoststore.Host{
		ID:            "host-1",
		UserID:        "alice",
		NotifierToken: "clnk_alice",
	})
	devices := &fakeDeviceStore{}
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "alice", PushToken: "ExpoPush[1]", Platform: "ios"})
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "alice", PushToken: "ExpoPush[2]", Platform: "android"})
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "bob", PushToken: "ExpoPush[other]", Platform: "ios"})
	pusher := &recordingPusher{}
	d := newDispatcher(t, hosts, devices, pusher)

	w := postNotification(t, d, "clnk_alice", notifier.Notification{
		SessionID: "s1",
		Kind:      notifier.KindIdle,
		Title:     "Agent finished",
		Body:      "Tap to see the result.",
	})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	sent := pusher.sentSnapshot()
	if len(sent) != 2 {
		t.Fatalf("got %d push messages, want 2 (alice's two devices)", len(sent))
	}
	for _, m := range sent {
		if m.Priority != PriorityHigh {
			t.Errorf("priority = %q, want high", m.Priority)
		}
		if m.Data["session_id"] != "s1" {
			t.Errorf("data.session_id = %v, want s1", m.Data["session_id"])
		}
		if m.Data["kind"] != string(notifier.KindIdle) {
			t.Errorf("data.kind = %v, want %q", m.Data["kind"], notifier.KindIdle)
		}
	}
}

func TestDispatcher_Handle_PassesThroughNotificationData(t *testing.T) {
	t.Parallel()
	hosts := newFakeHostLookup(hoststore.Host{UserID: "alice", NotifierToken: "clnk_alice"})
	devices := &fakeDeviceStore{}
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "alice", PushToken: "ExpoPush[1]", Platform: "ios"})
	pusher := &recordingPusher{}
	d := newDispatcher(t, hosts, devices, pusher)

	postNotification(t, d, "clnk_alice", notifier.Notification{
		SessionID: "s1",
		Kind:      notifier.KindPermission,
		Title:     "Permission requested",
		Body:      "bash",
		Data:      map[string]any{"request_id": "r1", "tool": "bash"},
	})

	sent := pusher.sentSnapshot()
	if len(sent) != 1 {
		t.Fatalf("got %d messages, want 1", len(sent))
	}
	if sent[0].Data["request_id"] != "r1" {
		t.Errorf("data.request_id = %v, want r1", sent[0].Data["request_id"])
	}
	if sent[0].Data["tool"] != "bash" {
		t.Errorf("data.tool = %v, want bash", sent[0].Data["tool"])
	}
}

func TestDispatcher_Handle_MissingBearerReturns401(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t, newFakeHostLookup(), &fakeDeviceStore{}, &recordingPusher{})
	w := postNotification(t, d, "", notifier.Notification{SessionID: "s1"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestDispatcher_Handle_UnknownBearerReturns401(t *testing.T) {
	t.Parallel()
	hosts := newFakeHostLookup() // empty
	pusher := &recordingPusher{}
	d := newDispatcher(t, hosts, &fakeDeviceStore{}, pusher)

	w := postNotification(t, d, "clnk_unknown", notifier.Notification{SessionID: "s1"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if got := len(pusher.sentSnapshot()); got != 0 {
		t.Errorf("pusher received %d messages despite 401", got)
	}
}

func TestDispatcher_Handle_NoDevicesStillReturns202(t *testing.T) {
	t.Parallel()
	hosts := newFakeHostLookup(hoststore.Host{UserID: "alice", NotifierToken: "clnk_alice"})
	pusher := &recordingPusher{}
	d := newDispatcher(t, hosts, &fakeDeviceStore{}, pusher)

	w := postNotification(t, d, "clnk_alice", notifier.Notification{SessionID: "s1", Kind: notifier.KindIdle})

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
	if got := len(pusher.sentSnapshot()); got != 0 {
		t.Errorf("pusher received %d messages with no registered devices", got)
	}
}

func TestDispatcher_Handle_PurgesDeviceNotRegistered(t *testing.T) {
	t.Parallel()
	hosts := newFakeHostLookup(hoststore.Host{UserID: "alice", NotifierToken: "clnk_alice"})
	devices := &fakeDeviceStore{}
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "alice", PushToken: "dead", Platform: "ios"})
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "alice", PushToken: "alive", Platform: "android"})
	pusher := &recordingPusher{
		ticketFor: func(m Message) Ticket {
			if m.To == "dead" {
				return Ticket{Status: "error", Details: struct {
					Error string `json:"error,omitempty"`
				}{Error: "DeviceNotRegistered"}}
			}
			return Ticket{Status: "ok"}
		},
	}
	d := newDispatcher(t, hosts, devices, pusher)

	postNotification(t, d, "clnk_alice", notifier.Notification{SessionID: "s1", Kind: notifier.KindIdle})

	remaining := devices.snapshot()
	if len(remaining) != 1 {
		t.Fatalf("got %d devices, want 1 (dead should be purged)", len(remaining))
	}
	if remaining[0].PushToken != "alive" {
		t.Errorf("kept push_token = %q, want alive", remaining[0].PushToken)
	}
}

func TestDispatcher_Handle_BadJSONReturns400(t *testing.T) {
	t.Parallel()
	hosts := newFakeHostLookup(hoststore.Host{UserID: "alice", NotifierToken: "clnk_alice"})
	d := newDispatcher(t, hosts, &fakeDeviceStore{}, &recordingPusher{})

	r := httptest.NewRequest(http.MethodPost, "/webhooks/notifications", bytes.NewReader([]byte("not json")))
	r.Header.Set("Authorization", "Bearer clnk_alice")
	w := httptest.NewRecorder()
	d.Handle(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDispatcher_HandleRegister_HappyPath(t *testing.T) {
	t.Parallel()
	devices := &fakeDeviceStore{}
	d := newDispatcher(t, newFakeHostLookup(), devices, &recordingPusher{})

	body, _ := json.Marshal(map[string]string{"push_token": "ExpoPush[1]", "platform": "ios"})
	r := httptest.NewRequest(http.MethodPost, "/devices", bytes.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: "alice"}))
	w := httptest.NewRecorder()
	d.HandleRegister(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	rows := devices.snapshot()
	if len(rows) != 1 {
		t.Fatalf("got %d devices, want 1", len(rows))
	}
	if rows[0].UserID != "alice" || rows[0].PushToken != "ExpoPush[1]" || rows[0].Platform != "ios" {
		t.Errorf("device = %+v, want {alice, ExpoPush[1], ios}", rows[0])
	}
}

func TestDispatcher_HandleRegister_RejectsBadPlatform(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t, newFakeHostLookup(), &fakeDeviceStore{}, &recordingPusher{})

	for _, platform := range []string{"", "web", "Iphone", "IOS"} {
		body, _ := json.Marshal(map[string]string{"push_token": "tok", "platform": platform})
		r := httptest.NewRequest(http.MethodPost, "/devices", bytes.NewReader(body))
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: "alice"}))
		w := httptest.NewRecorder()
		d.HandleRegister(w, r)
		if platform == "IOS" {
			// Case-insensitive normalization should accept this.
			if w.Code != http.StatusNoContent {
				t.Errorf("platform=%q: got %d, want 204 (case-folding)", platform, w.Code)
			}
			continue
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("platform=%q: got %d, want 400", platform, w.Code)
		}
	}
}

func TestDispatcher_HandleRegister_RejectsMissingToken(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t, newFakeHostLookup(), &fakeDeviceStore{}, &recordingPusher{})

	body, _ := json.Marshal(map[string]string{"push_token": "", "platform": "ios"})
	r := httptest.NewRequest(http.MethodPost, "/devices", bytes.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: "alice"}))
	w := httptest.NewRecorder()
	d.HandleRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDispatcher_HandleDeregister_HappyPath(t *testing.T) {
	t.Parallel()
	devices := &fakeDeviceStore{}
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "alice", PushToken: "tok", Platform: "ios"})
	d := newDispatcher(t, newFakeHostLookup(), devices, &recordingPusher{})

	r := httptest.NewRequest(http.MethodDelete, "/devices/tok", nil)
	r.SetPathValue("token", "tok")
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: "alice"}))
	w := httptest.NewRecorder()
	d.HandleDeregister(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if got := len(devices.snapshot()); got != 0 {
		t.Errorf("got %d devices after deregister, want 0", got)
	}
}

func TestDispatcher_HandleDeregister_OnlyAffectsCallerUser(t *testing.T) {
	t.Parallel()
	devices := &fakeDeviceStore{}
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "alice", PushToken: "tok", Platform: "ios"})
	_ = devices.UpsertDevice(context.Background(), Device{UserID: "bob", PushToken: "tok", Platform: "ios"})
	d := newDispatcher(t, newFakeHostLookup(), devices, &recordingPusher{})

	r := httptest.NewRequest(http.MethodDelete, "/devices/tok", nil)
	r.SetPathValue("token", "tok")
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: "alice"}))
	w := httptest.NewRecorder()
	d.HandleDeregister(w, r)

	rows := devices.snapshot()
	if len(rows) != 1 {
		t.Fatalf("got %d devices, want 1 (bob's untouched)", len(rows))
	}
	if rows[0].UserID != "bob" {
		t.Errorf("remaining device = %+v, want bob's row", rows[0])
	}
}
