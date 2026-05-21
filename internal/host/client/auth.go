package hostclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/acksell/clank/internal/agent"
)

// ListAuthProviders returns the providers this host can authenticate
// plus their current connection state. Wraps GET /auth/providers.
// backend, if non-empty, filters the result to providers consumed by
// that agent CLI (opencode | claude-code) — clients use this so the
// compose flow only surfaces providers relevant to the chosen
// backend.
func (c *HTTP) ListAuthProviders(ctx context.Context, backend agent.BackendType) ([]agent.ProviderAuthInfo, error) {
	var out []agent.ProviderAuthInfo
	path := "/auth/providers"
	if backend != "" {
		path += "?backend=" + url.QueryEscape(string(backend))
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
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

// StartOAuthCodeFlow kicks off an oauth-code flow for providerID
// (Anthropic Claude subscription today). The host spawns
// `claude setup-token` in a PTY and returns the verification URL the
// CLI prints, plus a flow_id for the subsequent SubmitAuthCode call.
func (c *HTTP) StartOAuthCodeFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := "/auth/" + url.PathEscape(providerID) + "/oauth/start"
	err := c.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
}

// SubmitAuthCode delivers the user-pasted authorization code to the
// host. The host writes it to the setup-token CLI's stdin, waits for
// the long-lived token to appear on stdout, and persists it.
// Synchronous: returns once the exchange completes (or fails).
func (c *HTTP) SubmitAuthCode(ctx context.Context, providerID, flowID, code string) error {
	path := "/auth/" + url.PathEscape(providerID) + "/oauth/submit"
	body := struct {
		FlowID string `json:"flow_id"`
		Code   string `json:"code"`
	}{FlowID: flowID, Code: code}
	return c.do(ctx, http.MethodPost, path, body, nil)
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
