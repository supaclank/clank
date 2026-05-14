package hostmux

import (
	"context"
	"net/http"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// handleSoftwareManifest serves this host's agent.SoftwareManifest.
// First call probes opencode (~500ms); subsequent calls are cached.
// 5s timeout bounds the first probe in case opencode hangs.
func (m *Mux) handleSoftwareManifest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, agent.GetSoftwareManifest(ctx))
}
