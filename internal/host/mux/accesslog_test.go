package hostmux

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestAccessLogWritesCompletionLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)

	h := accessLog(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/abc/messages?token=secret", nil))

	line := buf.String()
	want := regexp.MustCompile(`^hostmux: GET /sessions/abc/messages -> 418 in \d+(\.\d+)?[µmn]?s`)
	if !want.MatchString(line) {
		t.Fatalf("access log line %q does not match %v", line, want)
	}
	if bytes.Contains(buf.Bytes(), []byte("secret")) {
		t.Fatalf("access log leaked query string: %q", line)
	}
}

func TestAccessLogDefaultsTo200WithoutExplicitWriteHeader(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)

	h := accessLog(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if want := "hostmux: GET /status -> 200 in "; !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Fatalf("access log line %q missing %q", buf.String(), want)
	}
}

func TestAccessLogNilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()
	h := accessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// A handler that writes the body before calling WriteHeader has already
// implicitly sent 200 — the later WriteHeader is superfluous and must not
// flip the recorded status.
func TestAccessLogWriteThenLateWriteHeaderKeepsImplicit200(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)

	h := accessLog(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
		w.WriteHeader(http.StatusInternalServerError)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if want := "hostmux: GET /status -> 200 in "; !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Fatalf("access log line %q missing %q", buf.String(), want)
	}
}

// The SSE handler requires the writer to flush; the wrapper must keep
// that capability visible or /events would 500 behind the access log.
func TestAccessLogPreservesFlusher(t *testing.T) {
	t.Parallel()
	var flushed bool
	h := accessLog(log.New(&bytes.Buffer{}, "", 0))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapped writer lost http.Flusher")
			return
		}
		f.Flush()
		flushed = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
	if !flushed {
		t.Fatal("handler never flushed")
	}
	if !rec.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
}
