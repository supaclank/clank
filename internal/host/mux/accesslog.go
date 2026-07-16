package hostmux

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// accessLog wraps h so every request logs one completion line with
// method, path, status, and wall time. Streaming endpoints (SSE,
// tunnel) log when the stream ends, so a large duration there means a
// long-lived stream, not a slow handler. Query strings are omitted —
// they can carry tokens.
func accessLog(lg *log.Logger) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			h.ServeHTTP(sw, r)
			lg.Printf("hostmux: %s %s -> %d in %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
		})
	}
}

// statusWriter records the response status while passing Flush (SSE)
// and Hijack (websocket tunnel) through to the underlying writer.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hostmux: underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
