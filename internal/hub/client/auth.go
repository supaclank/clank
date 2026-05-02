package hubclient

// Hub-side auth proxy: TUI/CLI calls these; the hub forwards to the
// host's auth handler in the appropriate sandbox. All flows are
// host-scoped (auth.json lives per-sandbox), so the call surface
// hangs off HostClient rather than the top-level Client.

import (
	"context"
	"net/url"

	"github.com/acksell/clank/internal/agent"
)

// ListAuthProviders returns the auth-capable providers on this host
// plus their current connection state.
func (h *HostClient) ListAuthProviders(ctx context.Context) ([]agent.ProviderAuthInfo, error) {
	var out []agent.ProviderAuthInfo
	if err := h.c.get(ctx, h.base()+"/auth/providers", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StartAuthDeviceFlow kicks off device-flow auth for providerID and
// returns the user-facing fields the TUI shows (URL, user_code) plus
// a flow_id for subsequent status polls.
func (h *HostClient) StartAuthDeviceFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := h.base() + "/auth/" + url.PathEscape(providerID) + "/device/start"
	if err := h.c.post(ctx, path, nil, &out); err != nil {
		return agent.DeviceFlowStart{}, err
	}
	return out, nil
}

// SubmitAuthAPIKey stores an API key (and provider-specific metadata
// fields like Azure resourceName or Cloudflare accountId) for
// providerID on this host and returns a flow_id the caller polls
// until the OpenCode restart completes. metadata may be nil for
// providers that need only a key.
func (h *HostClient) SubmitAuthAPIKey(ctx context.Context, providerID, key string, metadata map[string]string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := h.base() + "/auth/" + url.PathEscape(providerID) + "/apikey"
	body := struct {
		Key      string            `json:"key"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}{Key: key, Metadata: metadata}
	if err := h.c.post(ctx, path, body, &out); err != nil {
		return agent.DeviceFlowStart{}, err
	}
	return out, nil
}

// AuthFlowStatus returns the current state of an in-progress flow
// (device or api-key — the endpoint is flow-type-agnostic). Pure read.
func (h *HostClient) AuthFlowStatus(ctx context.Context, providerID, flowID string) (agent.DeviceFlowStatus, error) {
	var out agent.DeviceFlowStatus
	path := h.base() + "/auth/" + url.PathEscape(providerID) + "/flow/status?flow_id=" + url.QueryEscape(flowID)
	if err := h.c.get(ctx, path, &out); err != nil {
		return agent.DeviceFlowStatus{}, err
	}
	return out, nil
}

// CancelAuthFlow signals the host to abort an in-progress flow.
// Idempotent for already-finished flows.
func (h *HostClient) CancelAuthFlow(ctx context.Context, providerID, flowID string) error {
	path := h.base() + "/auth/" + url.PathEscape(providerID) + "/flow?flow_id=" + url.QueryEscape(flowID)
	return h.c.do(ctx, "DELETE", path, nil, nil)
}

// DeleteAuthCredential logs the user out of providerID on this host
// and triggers an OpenCode restart so the change takes effect.
func (h *HostClient) DeleteAuthCredential(ctx context.Context, providerID string) error {
	path := h.base() + "/auth/" + url.PathEscape(providerID)
	return h.c.do(ctx, "DELETE", path, nil, nil)
}
