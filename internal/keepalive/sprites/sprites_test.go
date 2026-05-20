package sprites

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordedReq captures one inbound request to the fake Sprites socket.
type recordedReq struct {
	Method string
	Path   string
	Body   string
}

// startFakeSpritesSocket binds a tmp unix socket and serves http via
// httptest. Returns a listener wired to dial that socket, and a getter
// for the recorded inbound requests. No mocking — this is a real local
// HTTP server over a real unix socket.
func startFakeSpritesSocket(t *testing.T) (*Listener, func() []recordedReq) {
	t.Helper()

	// macOS limits AF_UNIX paths to ~104 bytes; t.TempDir() under
	// /var/folders/... typically blows that budget. /tmp is always
	// short enough.
	tmpDir, err := os.MkdirTemp("/tmp", "keepalive-sprites-*")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	socketPath := filepath.Join(tmpDir, "api.sock")
	uln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", socketPath, err)
	}

	var mu sync.Mutex
	var reqs []recordedReq

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 256)
		if r.Body != nil {
			b, _ := readAll(r.Body)
			body = b
		}
		mu.Lock()
		reqs = append(reqs, recordedReq{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = uln
	srv.Start()
	t.Cleanup(srv.Close)

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	return newWithClient(client, nil), func() []recordedReq {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedReq, len(reqs))
		copy(out, reqs)
		return out
	}
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf [4096]byte
	out := make([]byte, 0, 256)
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

// TestListener_TickRenewsLeaseOnRecentActivity is the happy path.
func TestListener_TickRenewsLeaseOnRecentActivity(t *testing.T) {
	t.Parallel()
	l, getReqs := startFakeSpritesSocket(t)
	l.Tick(context.Background(), time.Now())

	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", req.Method)
	}
	if want := tasksBasePath + "/" + leaseName; req.Path != want {
		t.Errorf("path = %s, want %s", req.Path, want)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatalf("body unmarshal: %v body=%q", err, req.Body)
	}
	if body["expire"] != leaseExpire {
		t.Errorf("body.expire = %q, want %q", body["expire"], leaseExpire)
	}
}

// TestListener_TickNoOpOnZeroTime pins the "no Bump yet" branch.
func TestListener_TickNoOpOnZeroTime(t *testing.T) {
	t.Parallel()
	l, getReqs := startFakeSpritesSocket(t)
	l.Tick(context.Background(), time.Time{})

	if got := getReqs(); len(got) != 0 {
		t.Fatalf("Tick(zero) sent %d requests, want 0", len(got))
	}
}

// TestListener_TickNoOpWhenIdle pins that activity older than
// idleThreshold stops renewals so the Sprites lease expires naturally.
func TestListener_TickNoOpWhenIdle(t *testing.T) {
	t.Parallel()
	l, getReqs := startFakeSpritesSocket(t)
	stale := time.Now().Add(-2 * idleThreshold)
	l.Tick(context.Background(), stale)

	if got := getReqs(); len(got) != 0 {
		t.Fatalf("Tick(stale) sent %d requests, want 0", len(got))
	}
}

// TestListener_CloseIssuesDelete pins the graceful release path.
func TestListener_CloseIssuesDelete(t *testing.T) {
	t.Parallel()
	l, getReqs := startFakeSpritesSocket(t)
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	if reqs[0].Method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", reqs[0].Method)
	}
	if want := tasksBasePath + "/" + leaseName; reqs[0].Path != want {
		t.Errorf("path = %s, want %s", reqs[0].Path, want)
	}
}

// TestListener_TickIdempotentRenew confirms that multiple Ticks within
// idleThreshold all PUT (renewing the lease), not POST (creating a new
// one). Sprites tasks are keyed by name so PUT is idempotent.
func TestListener_TickIdempotentRenew(t *testing.T) {
	t.Parallel()
	l, getReqs := startFakeSpritesSocket(t)
	now := time.Now()
	l.Tick(context.Background(), now)
	l.Tick(context.Background(), now)
	l.Tick(context.Background(), now)

	reqs := getReqs()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3", len(reqs))
	}
	for i, r := range reqs {
		if r.Method != http.MethodPut {
			t.Errorf("req %d method = %s, want PUT", i, r.Method)
		}
	}
}
