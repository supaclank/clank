package cloud

// auth.go — provider-auth client targeting the cloud gateway directly.
//
// Mirrors the surface of internal/host/client/auth.go (same URL paths,
// same JSON payloads, same agent types) but does not go through the
// laptop daemon's hub. Used by the TUI's Cloud panel for the
// "Connect provider (in sandbox)" flow.
//
// Follows the pattern established by clank push (internal/cli/clankcli/
// push.go): read GatewayURL + AccessToken from prefs.ActiveRemote() and
// hit the gateway directly with Authorization: Bearer <token>.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// AuthCaller wraps an *http.Client targeting the gateway, with the
// user's OAuth access token attached to each request. Construct one
// per modal invocation — it is cheap and stateless.
type AuthCaller struct {
	gatewayURL  string
	accessToken string
	http        *http.Client
}

// NewAuthCaller constructs a caller against gatewayURL using the given
// bearer access token. httpClient may be nil for the default.
func NewAuthCaller(gatewayURL, accessToken string, httpClient *http.Client) *AuthCaller {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &AuthCaller{
		gatewayURL:  strings.TrimRight(gatewayURL, "/"),
		accessToken: accessToken,
		http:        httpClient,
	}
}

// ListAuthProviders mirrors hostclient.HTTP.ListAuthProviders.
func (a *AuthCaller) ListAuthProviders(ctx context.Context, backend agent.BackendType) ([]agent.ProviderAuthInfo, error) {
	var out []agent.ProviderAuthInfo
	path := "/auth/providers"
	if backend != "" {
		path += "?backend=" + url.QueryEscape(string(backend))
	}
	err := a.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// StartAuthDeviceFlow mirrors daemonclient.HostClient.StartAuthDeviceFlow.
func (a *AuthCaller) StartAuthDeviceFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := "/auth/" + url.PathEscape(providerID) + "/device/start"
	err := a.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
}

// SubmitAuthAPIKey mirrors daemonclient.HostClient.SubmitAuthAPIKey.
func (a *AuthCaller) SubmitAuthAPIKey(ctx context.Context, providerID, key string, metadata map[string]string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := "/auth/" + url.PathEscape(providerID) + "/apikey"
	body := struct {
		Key      string            `json:"key"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}{Key: key, Metadata: metadata}
	err := a.do(ctx, http.MethodPost, path, body, &out)
	return out, err
}

// StartAuthOAuthCodeFlow mirrors daemonclient.HostClient.StartAuthOAuthCodeFlow.
func (a *AuthCaller) StartAuthOAuthCodeFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error) {
	var out agent.DeviceFlowStart
	path := "/auth/" + url.PathEscape(providerID) + "/oauth/start"
	err := a.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
}

// SubmitAuthCode mirrors daemonclient.HostClient.SubmitAuthCode.
func (a *AuthCaller) SubmitAuthCode(ctx context.Context, providerID, flowID, code string) error {
	path := "/auth/" + url.PathEscape(providerID) + "/oauth/submit"
	body := struct {
		FlowID string `json:"flow_id"`
		Code   string `json:"code"`
	}{FlowID: flowID, Code: code}
	return a.do(ctx, http.MethodPost, path, body, nil)
}

// AuthFlowStatus mirrors daemonclient.HostClient.AuthFlowStatus.
func (a *AuthCaller) AuthFlowStatus(ctx context.Context, providerID, flowID string) (agent.DeviceFlowStatus, error) {
	var out agent.DeviceFlowStatus
	path := "/auth/" + url.PathEscape(providerID) + "/flow/status?flow_id=" + url.QueryEscape(flowID)
	err := a.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// CancelAuthFlow mirrors daemonclient.HostClient.CancelAuthFlow.
func (a *AuthCaller) CancelAuthFlow(ctx context.Context, providerID, flowID string) error {
	path := "/auth/" + url.PathEscape(providerID) + "/flow?flow_id=" + url.QueryEscape(flowID)
	return a.do(ctx, http.MethodDelete, path, nil, nil)
}

func (a *AuthCaller) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.gatewayURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.accessToken)

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
