package hubmux

import (
	"net/http"

	"github.com/acksell/clank/internal/agent"
)

// handleVoiceStatus returns the current voice session state. The
// websocket audio handler stays on *hub.Service (HandleVoiceAudio)
// because it owns Service-internal singleton state plus a long-lived
// connection; mux.register() delegates the route directly.
func (m *Mux) handleVoiceStatus(w http.ResponseWriter, r *http.Request) {
	active, status := m.svc.VoiceStatus()
	if !active {
		writeJSON(w, http.StatusOK, map[string]string{"active": "false", "status": string(agent.VoiceStatusIdle)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"active": "true",
		"status": string(status),
	})
}
