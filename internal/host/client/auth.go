package hostclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/acksell/clank/internal/agent"
)

// ListAuthProviders returns the providers this host can authenticate
// plus their current connection state. Wraps GET /auth/providers.
func (c *HTTP) ListAuthProviders(ctx context.Context) ([]agent.ProviderAuthInfo, error) {
	var out []agent.ProviderAuthInfo
	err := c.do(ctx, http.MethodGet, "/auth/providers", nil, &out)
	return out, err
}

// StartDeviceFlow kicks off device-flow auth for providerID and
// returns the user-facing fields (URL, user_code) plus a flow_id
// for status polls.
func (c *HTTP) StartDeviceFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := "/auth/" + url.PathEscape(providerID) + "/device/start"
	err := c.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
}

// SubmitAPIKey stores an API key (plus any provider-specific
// metadata fields like Azure resourceName or Cloudflare accountId)
// for providerID and returns a flow_id the caller polls via
// FlowStatus to observe the post-write OpenCode restart. The
// metadata map may be nil for providers that need only a key.
func (c *HTTP) SubmitAPIKey(ctx context.Context, providerID, key string, metadata map[string]string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := "/auth/" + url.PathEscape(providerID) + "/apikey"
	body := struct {
		Key      string            `json:"key"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}{Key: key, Metadata: metadata}
	err := c.do(ctx, http.MethodPost, path, body, &out)
	return out, err
}

// FlowStatus reads the current state of an in-progress flow (device
// or api-key — the endpoint is flow-type-agnostic). Pure read —
// safe to call as fast as the caller wants.
func (c *HTTP) FlowStatus(ctx context.Context, providerID, flowID string) (agent.DeviceFlowStatus, error) {
	var out agent.DeviceFlowStatus
	path := "/auth/" + url.PathEscape(providerID) + "/flow/status?flow_id=" + url.QueryEscape(flowID)
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// CancelFlow signals the host to abort an in-progress flow.
// Idempotent for already-finished flows.
func (c *HTTP) CancelFlow(ctx context.Context, providerID, flowID string) error {
	path := "/auth/" + url.PathEscape(providerID) + "/flow?flow_id=" + url.QueryEscape(flowID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteAuthCredential removes the stored credential for providerID
// (logging the user out) and triggers an OpenCode server restart.
func (c *HTTP) DeleteAuthCredential(ctx context.Context, providerID string) error {
	path := "/auth/" + url.PathEscape(providerID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}
