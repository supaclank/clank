package hubmux

// HTTP routes for hub-side proxying of host auth flows. These are
// pure pass-throughs to the host's /auth/* endpoints — the hub holds
// no auth state of its own; auth.json lives in the sandbox.

import (
	"net/http"

	"github.com/acksell/clank/internal/agent"
)

func (m *Mux) registerAuth(mx *http.ServeMux) {
	mx.HandleFunc("GET /hosts/{hostname}/auth/providers", m.handleListAuthProvidersOnHost)
	mx.HandleFunc("POST /hosts/{hostname}/auth/{provider}/device/start", m.handleStartAuthDeviceFlowOnHost)
	mx.HandleFunc("POST /hosts/{hostname}/auth/{provider}/apikey", m.handleSubmitAuthAPIKeyOnHost)
	mx.HandleFunc("GET /hosts/{hostname}/auth/{provider}/flow/status", m.handleAuthFlowStatusOnHost)
	mx.HandleFunc("DELETE /hosts/{hostname}/auth/{provider}/flow", m.handleCancelAuthFlowOnHost)
	mx.HandleFunc("DELETE /hosts/{hostname}/auth/{provider}", m.handleDeleteAuthCredentialOnHost)
}

func (m *Mux) handleListAuthProvidersOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	infos, err := m.svc.ListAuthProvidersOnHost(r.Context(), hostname)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	if infos == nil {
		infos = []agent.ProviderAuthInfo{}
	}
	writeJSON(w, http.StatusOK, infos)
}

func (m *Mux) handleStartAuthDeviceFlowOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	provider := r.PathValue("provider")
	if provider == "" {
		writeBadRequest(w, "provider is required")
		return
	}
	start, err := m.svc.StartAuthDeviceFlowOnHost(r.Context(), hostname, provider)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, start)
}

func (m *Mux) handleSubmitAuthAPIKeyOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	provider := r.PathValue("provider")
	if provider == "" {
		writeBadRequest(w, "provider is required")
		return
	}
	var body struct {
		Key      string            `json:"key"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON: "+err.Error())
		return
	}
	start, err := m.svc.SubmitAuthAPIKeyOnHost(r.Context(), hostname, provider, body.Key, body.Metadata)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, start)
}

func (m *Mux) handleAuthFlowStatusOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	provider := r.PathValue("provider")
	flowID := r.URL.Query().Get("flow_id")
	if provider == "" || flowID == "" {
		writeBadRequest(w, "provider and flow_id are required")
		return
	}
	status, err := m.svc.AuthFlowStatusOnHost(r.Context(), hostname, provider, flowID)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (m *Mux) handleCancelAuthFlowOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	provider := r.PathValue("provider")
	flowID := r.URL.Query().Get("flow_id")
	if provider == "" || flowID == "" {
		writeBadRequest(w, "provider and flow_id are required")
		return
	}
	if err := m.svc.CancelAuthFlowOnHost(r.Context(), hostname, provider, flowID); err != nil {
		writeAuthErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mux) handleDeleteAuthCredentialOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	provider := r.PathValue("provider")
	if provider == "" {
		writeBadRequest(w, "provider is required")
		return
	}
	if err := m.svc.DeleteAuthCredentialOnHost(r.Context(), hostname, provider); err != nil {
		writeAuthErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeAuthErr maps auth errors back to HTTP statuses. The host wraps
// its auth sentinels into the JSON-error envelope; the hostclient
// preserves the wrapped chain. There aren't yet hub-package auth
// sentinels worth catching here, so we fall straight through to the
// generic service-error mapper which handles ErrHostNotRegistered
// (404) and the catch-all 500.
func writeAuthErr(w http.ResponseWriter, err error) {
	writeServiceErr(w, err)
}
