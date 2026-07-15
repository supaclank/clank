package hostmux

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
)

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

func (m *Mux) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"pid":     os.Getpid(),
		"version": daemonVersion(),
	})
}

// handleEvents is the global SSE stream. Subscribers receive every
// agent event the host produces, across every active session.
// Clients filter by SessionID on their side. Connection closes on
// client disconnect or host shutdown.
func (m *Mux) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errResp{Error: "streaming not supported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	subID, ch := m.svc.Subscribe()
	defer m.svc.Unsubscribe(subID)

	if err := writeSSE(w, "connected", map[string]string{"subscriber_id": subID}); err != nil {
		m.log.Printf("hostmux: SSE connected frame failed for sub=%s: %v", subID, err)
		return
	}
	flusher.Flush()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSE(w, string(evt.Type), evt); err != nil {
				m.log.Printf("hostmux: SSE write failed for sub=%s evt=%s: %v", subID, evt.Type, err)
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSE(w io.Writer, event string, data interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal SSE %q: %w", event, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return fmt.Errorf("write SSE %q frame: %w", event, err)
	}
	return nil
}
