package filepreview

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// pollInterval paces the on-disk stat of the watched file. Dependency-
// free (no fsnotify) and cheap at this rate for a single file.
const pollInterval = 500 * time.Millisecond

// pingInterval keeps a comment flowing so dead clients surface as write
// errors instead of leaking watch loops.
const pingInterval = 30 * time.Second

// handleEvents streams one SSE event per on-disk change of ?path= —
// the text shell's live-reload feed while an agent edits the file.
func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	info, err := h.root.Stat(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	lastMod, lastSize := info.ModTime(), info.Size()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	// Flush after every write: this server buffers ~4KB otherwise, and
	// the overlay proxy in front streams per-write (FlushInterval -1).
	if !writeSSE(w, rc, ": watching\n\n") {
		return
	}

	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if !writeSSE(w, rc, ": ping\n\n") {
				return
			}
		case <-poll.C:
			info, err := h.root.Stat(rel)
			if err != nil {
				// Editors and agents often replace files via rename; the
				// brief ENOENT between unlink and rename is not a change.
				continue
			}
			if info.ModTime().Equal(lastMod) && info.Size() == lastSize {
				continue
			}
			lastMod, lastSize = info.ModTime(), info.Size()
			if !writeSSE(w, rc, "data: change\n\n") {
				return
			}
		}
	}
}

func writeSSE(w io.Writer, rc *http.ResponseController, msg string) bool {
	if _, err := io.WriteString(w, msg); err != nil {
		return false
	}
	return rc.Flush() == nil
}
