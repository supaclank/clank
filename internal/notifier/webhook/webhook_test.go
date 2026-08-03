package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/notifier"
)

func newNotification(kind notifier.Kind) notifier.Notification {
	return notifier.Notification{
		SessionID:  "s1",
		Kind:       kind,
		Title:      "test",
		Body:       "body",
		OccurredAt: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
	}
}

// recordingServer is a real httptest.Server that captures every
// request received. Tests assert against it directly — no mocks.
type recordingServer struct {
	mu       sync.Mutex
	requests []*recordedRequest
	srv      *httptest.Server
}

type recordedRequest struct {
	method        string
	contentType   string
	authorization string
	body          notifier.Notification
}

func newRecordingServer(status func(attempt int) int) *recordingServer {
	rs := &recordingServer{}
	var attempt int64
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var n notifier.Notification
		_ = json.Unmarshal(body, &n)
		rs.mu.Lock()
		rs.requests = append(rs.requests, &recordedRequest{
			method:        r.Method,
			contentType:   r.Header.Get("Content-Type"),
			authorization: r.Header.Get("Authorization"),
			body:          n,
		})
		rs.mu.Unlock()
		code := http.StatusNoContent
		if status != nil {
			code = status(int(atomic.AddInt64(&attempt, 1)))
		}
		w.WriteHeader(code)
	}))
	return rs
}

func (rs *recordingServer) close() { rs.srv.Close() }

func (rs *recordingServer) snapshot() []*recordedRequest {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]*recordedRequest, len(rs.requests))
	copy(out, rs.requests)
	return out
}

func TestProvider_Send_PostsJSONWithBearer(t *testing.T) {
	t.Parallel()
	rs := newRecordingServer(nil)
	defer rs.close()

	p := New(rs.srv.URL, "clnk_test", nil)
	if err := p.Send(context.Background(), newNotification(notifier.KindIdle)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := rs.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.method)
	}
	if r.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", r.contentType)
	}
	if want := "Bearer clnk_test"; r.authorization != want {
		t.Errorf("Authorization = %q, want %q", r.authorization, want)
	}
	if r.body.Kind != notifier.KindIdle || r.body.SessionID != "s1" {
		t.Errorf("body decoded incorrectly: %+v", r.body)
	}
}

func TestProvider_Send_OmitsAuthHeaderWhenTokenEmpty(t *testing.T) {
	t.Parallel()
	rs := newRecordingServer(nil)
	defer rs.close()

	p := New(rs.srv.URL, "", nil)
	if err := p.Send(context.Background(), newNotification(notifier.KindIdle)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := rs.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	if reqs[0].authorization != "" {
		t.Errorf("Authorization = %q, want empty", reqs[0].authorization)
	}
}

func TestProvider_Send_RetriesOnceOn5xx(t *testing.T) {
	t.Parallel()
	rs := newRecordingServer(func(attempt int) int {
		if attempt == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})
	defer rs.close()

	p := New(rs.srv.URL, "clnk_test", nil)
	// Override the retry delay via a fresh struct — production
	// retryDelay is 1s, too slow for a unit test. The pkg-level const
	// is the production tuning; here we work around by accepting the
	// short delay.
	start := time.Now()
	err := p.Send(context.Background(), newNotification(notifier.KindIdle))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Send after retry: %v", err)
	}
	if len(rs.snapshot()) != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", len(rs.snapshot()))
	}
	if elapsed < retryDelay {
		t.Errorf("elapsed %v < retryDelay %v; retry happened too fast", elapsed, retryDelay)
	}
}

func TestProvider_Send_DoesNotRetryOn4xx(t *testing.T) {
	t.Parallel()
	rs := newRecordingServer(func(attempt int) int {
		return http.StatusUnauthorized
	})
	defer rs.close()

	p := New(rs.srv.URL, "clnk_test", nil)
	err := p.Send(context.Background(), newNotification(notifier.KindIdle))
	if err == nil {
		t.Fatal("Send should have returned an error on 401")
	}
	if got := len(rs.snapshot()); got != 1 {
		t.Errorf("got %d attempts, want 1 (4xx must not retry)", got)
	}
}

func TestProvider_Send_ReturnsErrorWhenBothAttemptsFail(t *testing.T) {
	t.Parallel()
	rs := newRecordingServer(func(attempt int) int {
		return http.StatusBadGateway
	})
	defer rs.close()

	p := New(rs.srv.URL, "clnk_test", nil)
	err := p.Send(context.Background(), newNotification(notifier.KindIdle))
	if err == nil {
		t.Fatal("Send should have returned an error when both attempts 5xx")
	}
	if got := len(rs.snapshot()); got != 2 {
		t.Errorf("got %d attempts, want 2", got)
	}
}
