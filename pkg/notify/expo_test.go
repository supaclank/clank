package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recordedAuth captures both the value and presence of the Authorization
// header so tests can distinguish a missing header from a present-but-empty
// one (http.Header.Get collapses both to "").
type recordedAuth struct {
	Value   string
	Present bool
}

// recordingExpo is a real httptest.Server that captures every request
// and returns caller-controlled tickets. Mirrors the recordingServer
// pattern used in the notifier/webhook tests.
type recordingExpo struct {
	mu             sync.Mutex
	requests       [][]Message
	authorizations []recordedAuth
	srv            *httptest.Server
}

func newRecordingExpo(t *testing.T, ticketFor func(m Message) Ticket) *recordingExpo {
	t.Helper()
	rx := &recordingExpo{}
	rx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msgs []Message
		if err := json.Unmarshal(body, &msgs); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		vals, present := r.Header["Authorization"]
		auth := recordedAuth{Present: present}
		if present && len(vals) > 0 {
			auth.Value = vals[0]
		}
		rx.mu.Lock()
		rx.requests = append(rx.requests, msgs)
		rx.authorizations = append(rx.authorizations, auth)
		rx.mu.Unlock()
		tickets := make([]Ticket, len(msgs))
		for i, m := range msgs {
			if ticketFor != nil {
				tickets[i] = ticketFor(m)
			} else {
				tickets[i] = Ticket{Status: "ok", ID: m.To}
			}
		}
		_ = json.NewEncoder(w).Encode(expoResponse{Data: tickets})
	}))
	t.Cleanup(rx.srv.Close)
	return rx
}

func (rx *recordingExpo) authsSnapshot() []recordedAuth {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	out := make([]recordedAuth, len(rx.authorizations))
	copy(out, rx.authorizations)
	return out
}

func (rx *recordingExpo) snapshot() [][]Message {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	out := make([][]Message, len(rx.requests))
	for i, r := range rx.requests {
		out[i] = append([]Message(nil), r...)
	}
	return out
}

func TestClient_Push_SingleBatch(t *testing.T) {
	t.Parallel()
	rx := newRecordingExpo(t, nil)
	c := NewWithEndpoint(rx.srv.URL, nil)

	msgs := []Message{
		{To: "a", Title: "hi", Body: "body"},
		{To: "b", Title: "hi2", Body: "body2"},
	}
	tickets, err := c.Push(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("got %d tickets, want 2", len(tickets))
	}
	for _, tk := range tickets {
		if tk.Status != "ok" {
			t.Errorf("ticket status = %q, want ok", tk.Status)
		}
	}
	batches := rx.snapshot()
	if len(batches) != 1 {
		t.Fatalf("got %d batches, want 1", len(batches))
	}
}

func TestClient_Push_ChunksAtMaxBatchSize(t *testing.T) {
	t.Parallel()
	rx := newRecordingExpo(t, nil)
	c := NewWithEndpoint(rx.srv.URL, nil)

	const total = maxBatchSize*2 + 13 // forces 3 batches
	msgs := make([]Message, total)
	for i := range msgs {
		msgs[i] = Message{To: "tok"}
	}
	tickets, err := c.Push(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(tickets) != total {
		t.Errorf("got %d tickets, want %d", len(tickets), total)
	}
	batches := rx.snapshot()
	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3 (chunked at %d)", len(batches), maxBatchSize)
	}
	if len(batches[0]) != maxBatchSize || len(batches[1]) != maxBatchSize || len(batches[2]) != 13 {
		t.Errorf("batch sizes = [%d %d %d], want [%d %d 13]", len(batches[0]), len(batches[1]), len(batches[2]), maxBatchSize, maxBatchSize)
	}
}

func TestClient_Push_EmptyInputIsNoop(t *testing.T) {
	t.Parallel()
	rx := newRecordingExpo(t, nil)
	c := NewWithEndpoint(rx.srv.URL, nil)
	tickets, err := c.Push(context.Background(), nil)
	if err != nil {
		t.Fatalf("Push(nil): %v", err)
	}
	if len(tickets) != 0 {
		t.Errorf("got %d tickets, want 0", len(tickets))
	}
	if got := len(rx.snapshot()); got != 0 {
		t.Errorf("got %d HTTP requests, want 0", got)
	}
}

func TestClient_Push_SurfacesPerMessageErrors(t *testing.T) {
	t.Parallel()
	rx := newRecordingExpo(t, func(m Message) Ticket {
		if m.To == "dead" {
			return Ticket{Status: "error", Details: struct {
				Error string `json:"error,omitempty"`
			}{Error: "DeviceNotRegistered"}}
		}
		return Ticket{Status: "ok"}
	})
	c := NewWithEndpoint(rx.srv.URL, nil)
	tickets, err := c.Push(context.Background(), []Message{
		{To: "alive"},
		{To: "dead"},
	})
	if err != nil {
		t.Fatalf("Push: %v (whole-batch should succeed)", err)
	}
	if tickets[0].IsDeviceNotRegistered() {
		t.Error("alive ticket falsely flagged DeviceNotRegistered")
	}
	if !tickets[1].IsDeviceNotRegistered() {
		t.Error("dead ticket missing DeviceNotRegistered flag")
	}
}

func TestClient_Push_SurfacesTopLevelExpoError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(expoResponse{
			Errors: []expoErr{{Code: "PUSH_TOO_MANY_NOTIFICATIONS", Message: "rate-limited"}},
		})
	}))
	defer srv.Close()
	c := NewWithEndpoint(srv.URL, nil)
	_, err := c.Push(context.Background(), []Message{{To: "tok"}})
	if err == nil {
		t.Fatal("expected error from top-level Expo error")
	}
}

func TestClient_Push_OmitsAuthHeaderWithoutAccessToken(t *testing.T) {
	t.Parallel()
	rx := newRecordingExpo(t, nil)
	c := NewWithEndpoint(rx.srv.URL, nil)
	if _, err := c.Push(context.Background(), []Message{{To: "tok"}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	auths := rx.authsSnapshot()
	if len(auths) != 1 {
		t.Fatalf("got %d requests, want 1", len(auths))
	}
	if auths[0].Present {
		t.Errorf("Authorization header present (value=%q), want absent (no token set)", auths[0].Value)
	}
}

func TestClient_Push_SendsAccessTokenWhenSet(t *testing.T) {
	t.Parallel()
	rx := newRecordingExpo(t, nil)
	c := NewWithEndpoint(rx.srv.URL, nil).WithAccessToken("expo_secret_xyz")
	if _, err := c.Push(context.Background(), []Message{{To: "tok"}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	auths := rx.authsSnapshot()
	if len(auths) != 1 {
		t.Fatalf("got %d requests, want 1", len(auths))
	}
	if !auths[0].Present {
		t.Fatalf("Authorization header absent, want present")
	}
	if want := "Bearer expo_secret_xyz"; auths[0].Value != want {
		t.Errorf("Authorization = %q, want %q", auths[0].Value, want)
	}
}

func TestClient_Push_FailsOnNon2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewWithEndpoint(srv.URL, nil)
	_, err := c.Push(context.Background(), []Message{{To: "tok"}})
	if err == nil {
		t.Fatal("expected error from 500 response")
	}
}
