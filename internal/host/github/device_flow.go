package github

// GitHub OAuth device flow: the user-facing connect path that lets a
// host obtain its own long-lived access token without ever seeing the
// OAuth App's client_secret.
//
// The wire-level RFC 8628 plumbing — POST /login/device/code, the
// polling loop against /login/oauth/access_token, slow_down/expired
// handling — lives in golang.org/x/oauth2. This file adds the
// per-host lifecycle on top: the single-slot flow registry, the
// FlowID-keyed status surface, credential persistence, and the
// terminal-state enum we expose to clients.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/oauth2"
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

// requestedScopes is the static set we ask for. `repo` covers private
// push + PR write; `read:user` lets us fetch the login for the UI.
// Single tier — no progressive consent in v1.
func requestedScopes() []string { return []string{"repo", "read:user"} }

// defaultPollSafetyMargin is added to the polling interval GitHub
// returns, matching the upstream Copilot plugin (3s). Defends against
// clock skew that would otherwise hit the token endpoint a hair too
// early and trigger slow_down. Per-Manager override via
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
// the GitHub page with UserCode pre-filled.
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
	// GitHub. The cancel is best-effort: the polling goroutine sees
	// ctx.Done at its next interval tick.
	m.cancelExistingFlow()

	cfg := m.oauth2Config()
	da, err := cfg.DeviceAuth(m.oauth2Context(ctx))
	if err != nil {
		return DeviceFlowStart{}, fmt.Errorf("device code: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return DeviceFlowStart{}, fmt.Errorf("device code: response missing required fields: %+v", da)
	}
	// GitHub returns verification_uri_complete; older clones might
	// not, in which case we synthesize one so the mobile UX still
	// pre-fills the code.
	if da.VerificationURIComplete == "" {
		da.VerificationURIComplete = da.VerificationURI + "?user_code=" + url.QueryEscape(da.UserCode)
	}
	// Add our clock-skew safety margin onto the polling cadence.
	// oauth2 honors da.Interval inside DeviceAccessToken.
	if m.pollSafetyMargin > 0 {
		da.Interval += int64(m.pollSafetyMargin / time.Second)
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

	go m.runDeviceFlow(flowCtx, flowID, cfg, da)

	return DeviceFlowStart{
		FlowID:                  flowID,
		UserCode:                da.UserCode,
		VerificationURI:         da.VerificationURI,
		VerificationURIComplete: da.VerificationURIComplete,
		ExpiresAt:               da.Expiry,
		Interval:                int(da.Interval),
	}, nil
}

// ConnectStatus returns the current state of flowID. Pure read.
// ErrUnknownFlow when the ID doesn't match the active slot (either
// it never existed, or a newer StartConnect replaced it).
func (m *Manager) ConnectStatus(_ context.Context, flowID string) (DeviceFlowStatus, error) {
	m.flowMu.Lock()
	defer m.flowMu.Unlock()
	// Evict stale terminal flows so a status poll after flowTTL
	// returns ErrUnknownFlow rather than the cached final state.
	m.gcFlowsLocked()
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
//
// First-writer-wins for non-pending states: once a flow has settled
// (e.g. CancelConnect set FlowCanceled while runDeviceFlow was mid-
// user-fetch), a late transition() from the goroutine won't clobber
// it. Symmetric with CancelConnect's "only transition from FlowPending"
// guard.
func (m *Manager) transition(flowID string, state DeviceFlowState, errMsg, login string) {
	m.flowMu.Lock()
	defer m.flowMu.Unlock()
	if m.currentFlow == nil || m.currentFlow.id != flowID {
		return // a newer flow has taken the slot — ignore.
	}
	if m.currentFlow.state != FlowPending {
		return // already settled — preserve the first terminal state.
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

// runDeviceFlow blocks in oauth2.DeviceAccessToken until the user
// completes (or denies) the flow on github.com, then captures the
// login + persists the credential. Errors are classified onto our
// DeviceFlowState enum.
func (m *Manager) runDeviceFlow(ctx context.Context, flowID string, cfg *oauth2.Config, da *oauth2.DeviceAuthResponse) {
	tok, err := cfg.DeviceAccessToken(m.oauth2Context(ctx), da)
	if err != nil {
		state, msg := classifyDeviceFlowErr(err)
		m.transition(flowID, state, msg, "")
		return
	}

	// Capture login + persist before transitioning so a status poll
	// observing "success" can trust the credential is on disk.
	login, userID, err := m.getAuthenticatedUser(ctx, tok.AccessToken)
	if err != nil {
		m.transition(flowID, FlowError, "fetch user: "+err.Error(), "")
		return
	}

	c := Credentials{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scopes:       parseScopes(tokenScopeString(tok)),
		GitHubLogin:  login,
		GitHubUserID: userID,
		InstalledAt:  time.Now().UTC(),
	}
	if !tok.Expiry.IsZero() {
		c.ExpiresAt = tok.Expiry.UTC()
	}
	if err := m.store.Write(c); err != nil {
		m.transition(flowID, FlowError, "persist credential: "+err.Error(), "")
		return
	}
	m.transition(flowID, FlowSuccess, "", login)
}

// classifyDeviceFlowErr maps oauth2's typed errors onto our flow
// states. ctx.Canceled → canceled; deadline exceeded means we passed
// the device-code expiry; *RetrieveError carries the OAuth2 error
// code (access_denied, expired_token) GitHub returned.
func classifyDeviceFlowErr(err error) (DeviceFlowState, string) {
	switch {
	case errors.Is(err, context.Canceled):
		return FlowCanceled, ""
	case errors.Is(err, context.DeadlineExceeded):
		return FlowExpired, "device code expired before authorization"
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch re.ErrorCode {
		case "access_denied":
			return FlowDenied, errMessage(re, "user denied authorization")
		case "expired_token":
			return FlowExpired, errMessage(re, "device code expired")
		}
		return FlowError, errMessage(re, "authorization failed")
	}
	return FlowError, err.Error()
}

// errMessage prefers the upstream ErrorDescription, falls back to the
// error code, then to the fallback string.
func errMessage(re *oauth2.RetrieveError, fallback string) string {
	if re.ErrorDescription != "" {
		return re.ErrorDescription
	}
	if re.ErrorCode != "" {
		return re.ErrorCode
	}
	return fallback
}

// tokenScopeString pulls the `scope` field out of the token response.
// oauth2.Token doesn't promote it to a struct field; it lives in the
// Raw map. GitHub returns a comma-separated string.
func tokenScopeString(tok *oauth2.Token) string {
	if tok == nil {
		return ""
	}
	if v, ok := tok.Extra("scope").(string); ok {
		return v
	}
	return ""
}

// parseScopes splits GitHub's comma-separated scope string into a
// slice. Empty input yields nil so the JSON encodes as "scopes":[]
// rather than the zero slice.
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
