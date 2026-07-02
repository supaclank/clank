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

// experienceAwareExpo mimics Expo's project check: requests whose
// tokens span more than one experience ID are rejected wholesale with
// PUSH_TOO_MANY_EXPERIENCE_IDS (status 400) plus a details map
// attributing each token, exactly like the hosted API. Requests within
// one experience succeed with one ticket per message (ID = token).
type experienceAwareExpo struct {
	mu       sync.Mutex
	requests [][]Message
	srv      *httptest.Server
}

func newExperienceAwareExpo(t *testing.T, expForToken map[string]string, ticketFor func(m Message) Ticket) *experienceAwareExpo {
	t.Helper()
	rx := &experienceAwareExpo{}
	rx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msgs []Message
		if err := json.Unmarshal(body, &msgs); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		rx.mu.Lock()
		rx.requests = append(rx.requests, msgs)
		rx.mu.Unlock()
		groups := map[string][]string{}
		for _, m := range msgs {
			groups[expForToken[m.To]] = append(groups[expForToken[m.To]], m.To)
		}
		if len(groups) > 1 {
			details, _ := json.Marshal(groups)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(expoResponse{Errors: []expoErr{{
				Code:    codeTooManyExperienceIDs,
				Message: "all push notification messages in the same request must be for the same project",
				Details: details,
			}}})
			return
		}
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

func (rx *experienceAwareExpo) snapshot() [][]Message {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	out := make([][]Message, len(rx.requests))
	for i, r := range rx.requests {
		out[i] = append([]Message(nil), r...)
	}
	return out
}

func TestClient_Push_SplitsMixedExperienceBatch(t *testing.T) {
	t.Parallel()
	experiences := map[string]string{
		"prod-a":   "@acksell/clank",
		"dev-b":    "@supaclank/clank",
		"prod-c":   "@acksell/clank",
		"dev-dead": "@supaclank/clank",
	}
	rx := newExperienceAwareExpo(t, experiences, func(m Message) Ticket {
		if m.To == "dev-dead" {
			return Ticket{Status: "error", Details: struct {
				Error string `json:"error,omitempty"`
			}{Error: "DeviceNotRegistered"}}
		}
		return Ticket{Status: "ok", ID: m.To}
	})
	c := NewWithEndpoint(rx.srv.URL, nil)

	msgs := []Message{{To: "prod-a"}, {To: "dev-b"}, {To: "prod-c"}, {To: "dev-dead"}}
	tickets, err := c.Push(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Push: %v (mixed-experience batch should be split, not fail)", err)
	}
	if len(tickets) != len(msgs) {
		t.Fatalf("got %d tickets, want %d", len(tickets), len(msgs))
	}
	// Tickets must line up with the original message order even though
	// delivery was regrouped per experience.
	for i, tok := range []string{"prod-a", "dev-b", "prod-c"} {
		idx := i
		if tok == "prod-c" {
			idx = 2
		}
		if tickets[idx].Status != "ok" || tickets[idx].ID != tok {
			t.Errorf("tickets[%d] = %+v, want ok ticket for %s", idx, tickets[idx], tok)
		}
	}
	if !tickets[3].IsDeviceNotRegistered() {
		t.Error("dev-dead ticket lost its DeviceNotRegistered flag through the split")
	}
	reqs := rx.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3 (1 rejected mixed + 2 per-experience)", len(reqs))
	}
	for _, req := range reqs[1:] {
		seen := map[string]bool{}
		for _, m := range req {
			seen[experiences[m.To]] = true
		}
		if len(seen) != 1 {
			t.Errorf("retry request mixes experiences: %v", req)
		}
	}
}

func TestClient_Push_PinnedExperienceDropsForeignTokens(t *testing.T) {
	t.Parallel()
	experiences := map[string]string{
		"prod-a": "@acksell/clank",
		"dev-b":  "@supaclank/clank",
	}
	rx := newExperienceAwareExpo(t, experiences, nil)
	c := NewWithEndpoint(rx.srv.URL, nil).WithExperienceID("@supaclank/clank")

	tickets, err := c.Push(context.Background(), []Message{{To: "prod-a"}, {To: "dev-b"}})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !tickets[0].IsMismatchedExperience() {
		t.Errorf("prod ticket = %+v, want MismatchedExperienceId (client pinned to @supaclank/clank)", tickets[0])
	}
	if tickets[1].Status != "ok" || tickets[1].ID != "dev-b" {
		t.Errorf("dev ticket = %+v, want ok", tickets[1])
	}
	reqs := rx.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2 (rejected mixed + pinned-experience retry)", len(reqs))
	}
	for _, m := range reqs[1] {
		if m.To == "prod-a" {
			t.Error("foreign token was re-sent despite the experience pin")
		}
	}
}

func TestClient_Push_SubBatchFailureOnlyAffectsItsExperience(t *testing.T) {
	t.Parallel()
	experiences := map[string]string{
		"prod-a": "@acksell/clank",
		"dev-b":  "@supaclank/clank",
	}
	// Like experienceAwareExpo, but single-experience retries for the
	// prod project blow up with a 500 — the dev group must still land.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msgs []Message
		_ = json.Unmarshal(body, &msgs)
		groups := map[string][]string{}
		for _, m := range msgs {
			groups[experiences[m.To]] = append(groups[experiences[m.To]], m.To)
		}
		if len(groups) > 1 {
			details, _ := json.Marshal(groups)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(expoResponse{Errors: []expoErr{{
				Code: codeTooManyExperienceIDs, Details: details,
			}}})
			return
		}
		if _, isProd := groups["@acksell/clank"]; isProd {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		tickets := make([]Ticket, len(msgs))
		for i, m := range msgs {
			tickets[i] = Ticket{Status: "ok", ID: m.To}
		}
		_ = json.NewEncoder(w).Encode(expoResponse{Data: tickets})
	}))
	defer srv.Close()
	c := NewWithEndpoint(srv.URL, nil)

	tickets, err := c.Push(context.Background(), []Message{{To: "prod-a"}, {To: "dev-b"}})
	if err != nil {
		t.Fatalf("Push: %v (one failing sub-batch should not fail the push)", err)
	}
	if tickets[0].Status != "error" {
		t.Errorf("prod ticket = %+v, want error (its sub-batch 500ed)", tickets[0])
	}
	if tickets[0].IsDeviceNotRegistered() || tickets[0].IsMismatchedExperience() {
		t.Errorf("prod ticket = %+v: transport failure must not be purge-flagged", tickets[0])
	}
	if tickets[1].Status != "ok" {
		t.Errorf("dev ticket = %+v, want ok", tickets[1])
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
