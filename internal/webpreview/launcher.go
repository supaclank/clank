package webpreview

import (
	"log"
	"net/http"
	"sync"
	"sync/atomic"
)

const LauncherSeenPath = "/__clank/launcher/seen"

type launcherState struct {
	mu   sync.Mutex // serializes acknowledgement attempts so persist runs at most once
	seen atomic.Bool
}

func newLauncherState(seen bool) *launcherState {
	s := &launcherState{}
	s.seen.Store(seen)
	return s
}

func (s *launcherState) isSeen() bool {
	return s.seen.Load()
}

// handleLauncherSeen only marks the launcher acknowledged after persist
// succeeds, so a failed write leaves state.seen false and a later
// request retries it instead of silently no-op'ing at 204.
func handleLauncherSeen(state *launcherState, persist func() error, lg *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.isSeen() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if persist != nil {
			if err := persist(); err != nil {
				lg.Printf("webpreview: persist launcher acknowledgement: %v", err)
				http.Error(w, "launcher opened, but saving the acknowledgement failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		state.seen.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}
}
