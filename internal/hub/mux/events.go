package hubmux

import (
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// daemonVersion is read once from the embedded build info. Returns
// "(devel)" for `go run`/unstamped builds, the module version for
// installed binaries, and the VCS revision when present. Avoids the
// previous hardcoded "0.1.0" lie.
var daemonVersion = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := info.Main.Version
	if v == "" {
		v = "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			if len(s.Value) > 12 {
				return v + "+" + s.Value[:12]
			}
			return v + "+" + s.Value
		}
	}
	return v
})

func (m *Mux) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"pid":     os.Getpid(),
		"uptime":  time.Since(m.svc.StartTime()).String(),
		"version": daemonVersion(),
	})
}

func (m *Mux) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pid":      os.Getpid(),
		"uptime":   time.Since(m.svc.StartTime()).String(),
		"sessions": m.svc.Sessions(),
	})
}

// handleEvents serves the SSE event stream to subscribers.
func (m *Mux) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	subID, ch, unsub := m.svc.Subscribe()
	defer unsub()

	if err := writeSSE(w, "connected", map[string]string{"subscriber_id": subID}); err != nil {
		m.log.Printf("hubmux: SSE connected frame failed for sub=%s: %v", subID, err)
		return
	}
	flusher.Flush()

	shutdownCtx := m.svc.ShutdownContext()
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSE(w, string(evt.Type), evt); err != nil {
				// Likely client disconnect; bail out so we stop pinning
				// the subscriber slot. unsub runs via defer.
				m.log.Printf("hubmux: SSE write failed for sub=%s evt=%s: %v", subID, evt.Type, err)
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-shutdownCtx.Done():
			return
		}
	}
}

// keep agent import needed for future use; currently the SSE writer
// only stringifies evt.Type, but other handlers read agent types.
var _ agent.EventType
