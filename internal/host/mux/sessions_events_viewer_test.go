package hostmux

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/store"
)

// newViewerTestServer builds a Service with a persisted session "s1"
// behind a live HTTP server (the SSE handler needs real streaming +
// client-disconnect semantics that httptest.NewRecorder can't provide).
func newViewerTestServer(t *testing.T) (*host.Service, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		SessionsStore:   st,
	})
	t.Cleanup(svc.Shutdown)
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID:      "s1",
		Backend: agent.BackendClaudeCode,
		Status:  agent.StatusIdle,
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	ts := httptest.NewServer(New(svc, nil).Handler())
	t.Cleanup(ts.Close)
	return svc, ts
}

// openSSE opens the per-session event stream and returns a cancel func
// that closes the client side of the connection.
func openSSE(t *testing.T, url string) (cancel func()) {
	t.Helper()
	ctx, ctxCancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		ctxCancel()
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ctxCancel()
		t.Fatalf("open SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		ctxCancel()
		t.Fatalf("SSE status = %d, want 200", resp.StatusCode)
	}
	return func() {
		ctxCancel()
		resp.Body.Close()
	}
}

func waitForViewers(svc *host.Service, sessionID string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if svc.SessionHasViewers(sessionID) == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return svc.SessionHasViewers(sessionID) == want
}

// A ?viewer=1 stream registers presence for its lifetime and releases
// it on disconnect — the signal the notifier uses to suppress idle
// pushes while the user is watching (guest-app overlay, mobile session
// view).
func TestSessionEvents_ViewerParamRegistersPresence(t *testing.T) {
	t.Parallel()
	svc, ts := newViewerTestServer(t)

	cancel := openSSE(t, ts.URL+"/sessions/s1/events?viewer=1")
	if !waitForViewers(svc, "s1", true, 2*time.Second) {
		t.Fatal("viewer stream open — SessionHasViewers must report true")
	}
	cancel()
	if !waitForViewers(svc, "s1", false, 2*time.Second) {
		t.Fatal("viewer stream closed — presence must be released")
	}
}

// A plain stream (no viewer param) is a machine consumer — the hub's
// relay client tails the same endpoint to mirror remote sessions, and
// counting it as a viewer would permanently suppress notifications for
// every relayed session.
func TestSessionEvents_PlainStreamRegistersNoPresence(t *testing.T) {
	t.Parallel()
	svc, ts := newViewerTestServer(t)

	cancel := openSSE(t, ts.URL+"/sessions/s1/events")
	defer cancel()
	// Give the handler time to have registered (wrongly) — presence must
	// stay false the whole way.
	time.Sleep(100 * time.Millisecond)
	if svc.SessionHasViewers("s1") {
		t.Fatal("plain stream must not register viewer presence")
	}
}
