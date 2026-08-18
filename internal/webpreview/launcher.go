package webpreview

import (
	"log"
	"net/http"
	"sync/atomic"
)

const LauncherSeenPath = "/__clank/launcher/seen"

type launcherState struct {
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

func handleLauncherSeen(state *launcherState, persist func() error, lg *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if state.seen.Swap(true) {
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
		w.WriteHeader(http.StatusNoContent)
	}
}
