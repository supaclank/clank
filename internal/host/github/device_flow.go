package github

// GitHub OAuth device flow: the user-facing connect path that lets a
// host obtain its own long-lived access token without ever seeing
// the OAuth App's client_secret. Same flow on mobile and TUI — only
// the client doing the polling differs.
//
// RFC 8628 reference, with GitHub's specifics layered on top:
//   1. POST https://github.com/login/device/code  → {device_code, user_code,
//      verification_uri, verification_uri_complete, expires_in, interval}
//   2. Display user_code + verification_uri_complete to the user.
//   3. POST https://github.com/login/oauth/access_token at `interval`
//      until success/error/expiry. Body: client_id + device_code +
//      grant_type=urn:ietf:params:oauth:grant-type:device_code.
//   4. On success: GET https://api.github.com/user to capture login,
//      persist the credential to the Store, transition to "success".
//
// Single-slot registry: a host has at most one in-flight flow at a
// time. A second StartConnect cancels the predecessor. This protects
// the sprite from accumulating goroutines when an impatient user
// re-taps the connect button.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// DeviceFlowState is the state a flow is in. Mirrors the agent.DeviceFlow
// alphabet but lives in this package so the github surface stays
// decoupled from the broader provider catalog.
type DeviceFlowState string

const (
	FlowPending  DeviceFlowState = "pending"
	FlowSuccess  DeviceFlowState = "success"
	FlowDenied   DeviceFlowState = "denied"
	FlowExpired  DeviceFlowState = "expired"
	FlowError    DeviceFlowState = "error"
	FlowCanceled DeviceFlowState = "canceled"
)

// scopeRepoAndUser is the static scope set we request. `repo` covers
// private push and PR write; `read:user` lets us fetch the login so
// the UI can render "@username". Single tier — no progressive
// consent in v1.
const scopeRepoAndUser = "repo read:user"

// defaultPollSafetyMargin is added to the polling interval GitHub
// returns, matching the upstream Copilot plugin (3s). Defends against
// clock skew that would otherwise have us hit the token endpoint a
// hair too early and get slow_down. Per-Manager override via
// SetPollSafetyMargin (tests use 0 for speed).
const defaultPollSafetyMargin = 3 * time.Second

// flowTTL is how long a finished flow's state lingers so a status
// poll after the terminal transition can still observe the result.
const flowTTL = 10 * time.Minute

// Errors returned by the device-flow surface. Map to specific HTTP
// statuses in the mux handlers — see github_connect.go.
var (
	// ErrNotConfigured is returned by StartConnect when the host has
	// no CLANK_GITHUB_OAUTH_CLIENT_ID. The mux maps it to 503.
	ErrNotConfigured = errors.New("github: client_id not configured (set CLANK_GITHUB_OAUTH_CLIENT_ID)")

	// ErrUnknownFlow is returned when a status/cancel call targets a
	// flow the manager has no record of (typo, expired TTL, or a
	// fresh start has bumped the slot).
	ErrUnknownFlow = errors.New("github: unknown flow id")
)

// DeviceFlowStart is the response from StartConnect. UserCode and
// VerificationURIComplete are what the client (mobile/TUI) shows to
// the user — opening VerificationURIComplete in a browser brings up
// the GitHub page with UserCode pre-filled, so the user only has to
// click Continue and Authorize.
type DeviceFlowStart struct {
	FlowID                  string    `json:"flow_id"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete"`
	ExpiresAt               time.Time `json:"expires_at"`
	Interval                int       `json:"interval"`
}

// DeviceFlowStatus is the polled state of an in-progress flow.
// GitHubLogin is populated only when State is "success" — same atomic
// step that wrote the credential file fetched the login.
type DeviceFlowStatus struct {
	State       DeviceFlowState `json:"state"`
	Error       string          `json:"error,omitempty"`
	GitHubLogin string          `json:"github_login,omitempty"`
}

// flowState is the in-memory record for one in-progress flow.
// Mutated by the polling goroutine; read under flowMu in
// ConnectStatus and CancelConnect.
type flowState struct {
	id          string
	state       DeviceFlowState
	err         string
	githubLogin string
	cancel      context.CancelFunc
	finishedAt  time.Time
}

// StartConnect kicks off the device flow against GitHub and spawns a
// goroutine that polls until completion. Returns the user-facing
// fields the client renders. Any in-flight flow is canceled first —
// single-slot registry per host.
func (m *Manager) StartConnect(ctx context.Context) (DeviceFlowStart, error) {
	if !m.IsAvailable() {
		return DeviceFlowStart{}, ErrNotConfigured
	}

	// Cancel any predecessor before opening the network call to
	// GitHub. The cancel is best-effort: the goroutine sees ctx.Done
	// at its next interval tick.
	m.cancelExistingFlow()

	code, err := m.requestDeviceCode(ctx)
	if err != nil {
		return DeviceFlowStart{}, err
	}

	flowID := ulid.Make().String()
	flowCtx, cancel := context.WithCancel(context.Background())
	m.flowMu.Lock()
	m.currentFlow = &flowState{
		id:     flowID,
		state:  FlowPending,
		cancel: cancel,
	}
	m.flowMu.Unlock()

	go m.runDeviceFlow(flowCtx, flowID, code)

	return DeviceFlowStart{
		FlowID:                  flowID,
		UserCode:                code.UserCode,
		VerificationURI:         code.VerificationURI,
		VerificationURIComplete: code.VerificationURIComplete,
		ExpiresAt:               time.Now().Add(time.Duration(code.ExpiresIn) * time.Second),
		Interval:                code.Interval,
	}, nil
}

// ConnectStatus returns the current state of flowID. Pure read.
// ErrUnknownFlow when the ID doesn't match the active slot (either
// it never existed, or a newer StartConnect replaced it).
func (m *Manager) ConnectStatus(_ context.Context, flowID string) (DeviceFlowStatus, error) {
	m.flowMu.Lock()
	defer m.flowMu.Unlock()
	if m.currentFlow == nil || m.currentFlow.id != flowID {
		return DeviceFlowStatus{}, ErrUnknownFlow
	}
	return DeviceFlowStatus{
		State:       m.currentFlow.state,
		Error:       m.currentFlow.err,
		GitHubLogin: m.currentFlow.githubLogin,
	}, nil
}

// CancelConnect signals the polling goroutine to stop and transitions
// the flow to "canceled". Idempotent — calling cancel on a flow
// that's already terminal is a no-op.
func (m *Manager) CancelConnect(_ context.Context, flowID string) error {
	m.flowMu.Lock()
	defer m.flowMu.Unlock()
	if m.currentFlow == nil || m.currentFlow.id != flowID {
		return ErrUnknownFlow
	}
	if m.currentFlow.state == FlowPending {
		m.currentFlow.state = FlowCanceled
		m.currentFlow.finishedAt = time.Now()
		m.currentFlow.cancel()
	}
	return nil
}

// cancelExistingFlow tears down the prior flow (if any) before
// starting a new one. Called from StartConnect, before any network
// activity, so the new flow doesn't race with stale polling.
func (m *Manager) cancelExistingFlow() {
	m.flowMu.Lock()
	defer m.flowMu.Unlock()
	if m.currentFlow == nil {
		return
	}
	if m.currentFlow.state == FlowPending {
		m.currentFlow.state = FlowCanceled
		m.currentFlow.finishedAt = time.Now()
	}
	m.currentFlow.cancel()
	m.currentFlow = nil
}

// transition mutates the current flow's state under flowMu. Records
// finishedAt on terminal states so a future StartConnect can observe
// "the previous flow is dead" and replace the slot cleanly.
func (m *Manager) transition(flowID string, state DeviceFlowState, errMsg, login string) {
	m.flowMu.Lock()
	defer m.flowMu.Unlock()
	if m.currentFlow == nil || m.currentFlow.id != flowID {
		return // a newer flow has taken the slot — ignore.
	}
	m.currentFlow.state = state
	if errMsg != "" {
		m.currentFlow.err = errMsg
	}
	if login != "" {
		m.currentFlow.githubLogin = login
	}
	switch state {
	case FlowSuccess, FlowExpired, FlowDenied, FlowError, FlowCanceled:
		m.currentFlow.finishedAt = time.Now()
	}
	m.gcFlowsLocked()
}

// gcFlowsLocked clears the slot when the current flow is past TTL,
// so a fresh StartConnect doesn't observe a stale terminal state as
// the "current" flow. Must be called with flowMu held.
func (m *Manager) gcFlowsLocked() {
	if m.currentFlow == nil {
		return
	}
	if !m.currentFlow.finishedAt.IsZero() && time.Since(m.currentFlow.finishedAt) > flowTTL {
		m.currentFlow = nil
	}
}

// --- internal: GitHub network calls ---

type deviceCodeResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// requestDeviceCode hits POST /login/device/code on the auth base URL.
// Form-encoded body per GitHub's docs (the JSON path is undocumented
// for this endpoint and has surfaced quirks historically). Returns
// the parsed response — DeviceCode + Interval are required; we
// surface a clear error if either is missing.
func (m *Manager) requestDeviceCode(ctx context.Context) (deviceCodeResp, error) {
	form := url.Values{}
	form.Set("client_id", m.clientID)
	form.Set("scope", scopeRepoAndUser)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.authBaseURL+"/login/device/code", strings.NewReader(form.Encode()))
	if err != nil {
		return deviceCodeResp{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var out deviceCodeResp
	if err := m.doJSON(req, &out); err != nil {
		return deviceCodeResp{}, fmt.Errorf("device code: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return deviceCodeResp{}, fmt.Errorf("device code: response missing required fields: %+v", out)
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	// GitHub returns verification_uri_complete; older clones might
	// not, in which case we synthesize one so the mobile UX still
	// pre-fills the code.
	if out.VerificationURIComplete == "" {
		out.VerificationURIComplete = out.VerificationURI + "?user_code=" + url.QueryEscape(out.UserCode)
	}
	return out, nil
}

type tokenResp struct {
	AccessToken      string `json:"access_token,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	Scope            string `json:"scope,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// pollToken hits POST /login/oauth/access_token once. Returns
// (token, status, err) where status is a stable string we drive the
// loop off of: "success", "pending", "slow_down", "denied", "expired",
// "error".
func (m *Manager) pollToken(ctx context.Context, deviceCode string) (tokenResp, string, error) {
	form := url.Values{}
	form.Set("client_id", m.clientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.authBaseURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResp{}, "error", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// GitHub's access_token endpoint returns 200 with an `error` body
	// for the polling-phase states (authorization_pending, slow_down,
	// access_denied, expired_token). Treat them as success-status from
	// the HTTP layer and dispatch on the JSON payload.
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return tokenResp{}, "error", fmt.Errorf("token poll: %w", err)
	}
	defer resp.Body.Close()
	var out tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return tokenResp{}, "error", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken != "" {
		return out, "success", nil
	}
	switch out.Error {
	case "authorization_pending":
		return out, "pending", nil
	case "slow_down":
		return out, "slow_down", nil
	case "access_denied":
		return out, "denied", nil
	case "expired_token":
		return out, "expired", nil
	default:
		return out, "error", nil
	}
}

// runDeviceFlow polls the token endpoint until success/error/expiry.
// On success, fetches the user's login, persists the credential, and
// transitions to "success" with the login in the status payload.
//
// Sleep cadence follows RFC 8628: respect the response's interval,
// add 5s on slow_down.
func (m *Manager) runDeviceFlow(ctx context.Context, flowID string, code deviceCodeResp) {
	interval := time.Duration(code.Interval)*time.Second + m.pollSafetyMargin
	expiresAt := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)

	for {
		if time.Now().After(expiresAt) {
			m.transition(flowID, FlowExpired, "device code expired before authorization", "")
			return
		}
		select {
		case <-ctx.Done():
			m.transition(flowID, FlowCanceled, "", "")
			return
		case <-time.After(interval):
		}

		tok, status, err := m.pollToken(ctx, code.DeviceCode)
		if err != nil {
			m.transition(flowID, FlowError, err.Error(), "")
			return
		}
		switch status {
		case "pending":
			continue
		case "slow_down":
			interval = interval + 5*time.Second
			continue
		case "denied":
			m.transition(flowID, FlowDenied, "user denied authorization", "")
			return
		case "expired":
			m.transition(flowID, FlowExpired, "device code expired", "")
			return
		case "error":
			msg := tok.ErrorDescription
			if msg == "" {
				msg = tok.Error
			}
			if msg == "" {
				msg = "authorization failed"
			}
			m.transition(flowID, FlowError, msg, "")
			return
		case "success":
			// Capture login + persist before transitioning so a
			// status poll observing "success" can trust the
			// credential is on disk.
			login, userID, err := m.getAuthenticatedUser(ctx, tok.AccessToken)
			if err != nil {
				m.transition(flowID, FlowError, "fetch user: "+err.Error(), "")
				return
			}
			c := Credentials{
				AccessToken:  tok.AccessToken,
				RefreshToken: tok.RefreshToken,
				Scopes:       parseScopes(tok.Scope),
				GitHubLogin:  login,
				GitHubUserID: userID,
				InstalledAt:  time.Now().UTC(),
			}
			if tok.ExpiresIn > 0 {
				c.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
			}
			if err := m.store.Write(c); err != nil {
				m.transition(flowID, FlowError, "persist credential: "+err.Error(), "")
				return
			}
			m.transition(flowID, FlowSuccess, "", login)
			return
		}
	}
}

// parseScopes splits a comma-separated scope string (the GitHub
// access_token response format) into a slice. Empty input yields nil
// so the JSON encodes as "scopes":[] rather than the zero slice.
func parseScopes(s string) []string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
