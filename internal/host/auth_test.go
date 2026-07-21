package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// newTestAuthManager constructs an AuthManager pinned to a temp dir
// (so writes don't touch the real $HOME), a no-op restart hook, and an
// empty env (so a dev machine's exported API keys can't leak into
// status assertions; tests that want env credentials set lookupEnv).
// homeDir is exposed for assertions on the on-disk auth.json.
func newTestAuthManager(t *testing.T) (*AuthManager, string) {
	t.Helper()
	dir := t.TempDir()
	a := &AuthManager{
		homeDir:   dir,
		restart:   func(context.Context) error { return nil },
		flows:     make(map[string]*flowState),
		httpc:     &http.Client{Timeout: 5 * time.Second},
		lookupEnv: func(string) string { return "" },
	}
	return a, dir
}

// mapEnv returns a lookupEnv over a fixed map — the test stand-in for
// os.Getenv.
func mapEnv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestAuthManager_ListProviders_EmptyFile(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	infos, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	// Catalog has the full list; on a fresh sandbox none are connected.
	if len(infos) == 0 {
		t.Fatalf("expected non-empty provider list")
	}
	for _, p := range infos {
		if p.Connected {
			t.Errorf("expected %s disconnected on fresh sandbox, got connected", p.ProviderID)
		}
	}
	// github-copilot must be present and surface as a device flow.
	var copilot agent.ProviderAuthInfo
	for _, p := range infos {
		if p.ProviderID == ProviderGitHubCopilot {
			copilot = p
		}
	}
	if copilot.ProviderID != ProviderGitHubCopilot {
		t.Fatalf("expected github-copilot in catalog")
	}
	if copilot.AuthType != "device" {
		t.Errorf("expected github-copilot AuthType=device, got %s", copilot.AuthType)
	}
}

func TestAuthManager_WriteAndReadAuthJSON(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)

	cred := agent.AuthCredential{
		Type:    "oauth",
		Refresh: "tok",
		Access:  "tok",
		Expires: 0,
	}
	if err := a.writeAuthJSON("github-copilot", cred); err != nil {
		t.Fatalf("writeAuthJSON: %v", err)
	}

	// File should exist with the expected layout.
	path := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var got map[string]agent.AuthCredential
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode auth.json: %v", err)
	}
	if got["github-copilot"].Refresh != "tok" {
		t.Errorf("expected refresh=tok, got %+v", got["github-copilot"])
	}

	// ListProviders should now report github-copilot as connected.
	// Look it up by ID rather than indexing — catalog ordering is a
	// presentation choice (Claude Code is listed before OpenCode) and
	// shouldn't be assumed by tests.
	infos, _ := a.ListProviders(context.Background(), "")
	var copilot *agent.ProviderAuthInfo
	for i := range infos {
		if infos[i].ProviderID == "github-copilot" {
			copilot = &infos[i]
			break
		}
	}
	if copilot == nil {
		t.Fatalf("github-copilot not found in catalog")
	}
	if !copilot.Connected {
		t.Errorf("expected github-copilot connected after write")
	}

	// File mode should be 0o600 (perm-restricted credentials).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected 0o600, got %o", perm)
	}
}

// TestAuthManager_OAuthCredentialOnDiskMatchesOpenCodeSchema pins the
// invariant that the JSON we write to ~/.local/share/opencode/auth.json
// satisfies opencode's OAuth schema validator (see
// packages/opencode/src/auth/index.ts upstream): `expires` is REQUIRED.
//
// Regression coverage for a silent-drop bug: AuthCredential.Expires used
// to carry json:"expires,omitempty", which omits the field when zero
// (the upstream-blessed value for Copilot tokens that have no tracked
// TTL). opencode's schema then rejected the entry, the credential never
// reached the provider plugin, and the only thing showing up in the
// model picker was the OpenCode Zen free tier.
func TestAuthManager_OAuthCredentialOnDiskMatchesOpenCodeSchema(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)

	cred := agent.AuthCredential{
		Type:    "oauth",
		Refresh: "gho_tok",
		Access:  "gho_tok",
		Expires: 0,
	}
	if err := a.writeAuthJSON("github-copilot", cred); err != nil {
		t.Fatalf("writeAuthJSON: %v", err)
	}

	path := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	// Decode into a generic map so we observe what's literally on disk —
	// not what struct unmarshalling would re-default.
	var raw map[string]map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode auth.json: %v", err)
	}
	entry, ok := raw["github-copilot"]
	if !ok {
		t.Fatalf("github-copilot entry missing from auth.json")
	}
	for _, required := range []string{"type", "refresh", "access", "expires"} {
		if _, present := entry[required]; !present {
			t.Errorf("auth.json[github-copilot] missing required field %q. opencode's OAuth schema rejects entries without it. Got: %v", required, entry)
		}
	}
}

func TestAuthManager_DeleteCredentialRoundTrip(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	cred := agent.AuthCredential{Type: "oauth", Refresh: "tok", Access: "tok"}
	if err := a.writeAuthJSON("github-copilot", cred); err != nil {
		t.Fatalf("write: %v", err)
	}

	var restartCalls int32
	a.restart = func(context.Context) error { atomic.AddInt32(&restartCalls, 1); return nil }

	if err := a.DeleteCredential(context.Background(), "github-copilot"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if got := atomic.LoadInt32(&restartCalls); got != 1 {
		t.Errorf("expected 1 restart call, got %d", got)
	}

	infos, _ := a.ListProviders(context.Background(), "")
	if infos[0].Connected {
		t.Errorf("expected disconnected after delete")
	}
}

func TestAuthManager_StartDeviceFlow_RejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	if _, err := a.StartDeviceFlow(context.Background(), "unknown-provider"); err == nil {
		t.Fatalf("expected ErrUnknownProvider, got nil")
	}
}

// StartDeviceFlow with an api-typed provider must reject — device
// flow is only for the github-copilot entry. Catches a regression
// where the catalog lookup might return an api provider but the
// auth-type guard miss it.
func TestAuthManager_StartDeviceFlow_RejectsAPIProvider(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	if _, err := a.StartDeviceFlow(context.Background(), "openai"); err == nil {
		t.Fatalf("expected error when starting device flow on openai (api type), got nil")
	}
}

// SubmitAPIKey on an api-typed provider must walk the full flow —
// pending → authorized (auth.json written) → success (restart hook
// called).
func TestAuthManager_SubmitAPIKey_HappyPath(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)
	var restartCalls int32
	a.restart = func(context.Context) error { atomic.AddInt32(&restartCalls, 1); return nil }

	flowID, err := a.SubmitAPIKey(context.Background(), "openai", "sk-test-12345", nil)
	if err != nil {
		t.Fatalf("SubmitAPIKey: %v", err)
	}
	if flowID == "" {
		t.Fatalf("expected non-empty flow_id")
	}

	deadline := time.Now().Add(5 * time.Second)
	var finalState agent.DeviceFlowState
	for time.Now().Before(deadline) {
		status, err := a.GetFlowStatus(context.Background(), flowID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		finalState = status.State
		if status.State == agent.DeviceFlowSuccess ||
			status.State == agent.DeviceFlowError {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalState != agent.DeviceFlowSuccess {
		t.Fatalf("expected success, got %s", finalState)
	}
	if got := atomic.LoadInt32(&restartCalls); got != 1 {
		t.Errorf("expected 1 restart call, got %d", got)
	}

	// auth.json should contain the api credential.
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var stored map[string]agent.AuthCredential
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode auth.json: %v", err)
	}
	if got := stored["openai"]; got.Type != "api" || got.Key != "sk-test-12345" {
		t.Errorf("expected openai api/sk-test-12345, got %+v", got)
	}
}

// Empty / whitespace keys must be rejected before the goroutine
// spawns — otherwise we'd happily write an empty credential to
// auth.json and OpenCode would fail at request time with a less
// useful error.
func TestAuthManager_SubmitAPIKey_RejectsBlankKey(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	if _, err := a.SubmitAPIKey(context.Background(), "openai", "", nil); err == nil {
		t.Errorf("expected ErrInvalidAPIKey on empty key")
	}
	if _, err := a.SubmitAPIKey(context.Background(), "openai", "   ", nil); err == nil {
		t.Errorf("expected ErrInvalidAPIKey on whitespace key")
	}
}

// SubmitAPIKey on a device-typed provider must reject — github-copilot
// requires the device flow.
func TestAuthManager_SubmitAPIKey_RejectsDeviceProvider(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	if _, err := a.SubmitAPIKey(context.Background(), "github-copilot", "ghp_test", nil); err == nil {
		t.Errorf("expected error when submitting api key for github-copilot (device type)")
	}
}

// Multi-field providers (Azure, Cloudflare) must round-trip both
// the key and the prompt metadata to auth.json.
func TestAuthManager_SubmitAPIKey_WithMetadata(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)
	a.restart = func(context.Context) error { return nil }

	flowID, err := a.SubmitAPIKey(context.Background(), "azure", "az-key-123", map[string]string{
		"resourceName": "my-models",
	})
	if err != nil {
		t.Fatalf("SubmitAPIKey: %v", err)
	}

	// Wait for the flow to complete.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := a.GetFlowStatus(context.Background(), flowID)
		if status.State == agent.DeviceFlowSuccess || status.State == agent.DeviceFlowError {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var stored map[string]agent.AuthCredential
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode auth.json: %v", err)
	}
	got := stored["azure"]
	if got.Type != "api" || got.Key != "az-key-123" {
		t.Errorf("expected azure api/az-key-123, got %+v", got)
	}
	if got.Metadata["resourceName"] != "my-models" {
		t.Errorf("expected resourceName=my-models, got %+v", got.Metadata)
	}
}

// Missing required prompts must reject before the goroutine spawns.
// Otherwise we'd write a half-baked credential to auth.json.
func TestAuthManager_SubmitAPIKey_RejectsMissingPrompt(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	// Azure requires resourceName; submit without it.
	if _, err := a.SubmitAPIKey(context.Background(), "azure", "az-key", nil); err == nil {
		t.Errorf("expected ErrMissingPrompt on azure without resourceName")
	}
	// Empty value should also reject.
	if _, err := a.SubmitAPIKey(context.Background(), "azure", "az-key", map[string]string{
		"resourceName": "   ",
	}); err == nil {
		t.Errorf("expected ErrMissingPrompt on whitespace resourceName")
	}
}

// Cloudflare AI Gateway has two prompts (accountId + gatewayId).
// Only providing one should still reject.
func TestAuthManager_SubmitAPIKey_RejectsPartialPrompts(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	if _, err := a.SubmitAPIKey(context.Background(), "cloudflare-ai-gateway", "cf-key", map[string]string{
		"accountId": "abc123",
		// gatewayId missing
	}); err == nil {
		t.Errorf("expected ErrMissingPrompt when gatewayId omitted")
	}
}

// Unknown metadata keys (e.g. typos) must be silently dropped at
// the manager boundary — a misspelled "resourcename" shouldn't end
// up in auth.json next to the real "resourceName".
func TestAuthManager_SubmitAPIKey_FiltersUnknownMetadata(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)
	a.restart = func(context.Context) error { return nil }

	_, err := a.SubmitAPIKey(context.Background(), "azure", "az-key", map[string]string{
		"resourceName": "my-models",
		"unrelated":    "should be dropped",
	})
	if err != nil {
		t.Fatalf("SubmitAPIKey: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Wait for write to complete.
		path := filepath.Join(home, ".local", "share", "opencode", "auth.json")
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	data, _ := os.ReadFile(authPath)
	var stored map[string]agent.AuthCredential
	_ = json.Unmarshal(data, &stored)
	if _, ok := stored["azure"].Metadata["unrelated"]; ok {
		t.Errorf("expected unrelated metadata key to be filtered, but it persisted")
	}
}

// ListProviders must include every catalog entry, marking only the
// stored ones as connected.
func TestAuthManager_ListProviders_IncludesEntireCatalog(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	// Pre-populate auth.json with one api credential to test the
	// connected-state propagation.
	if err := a.writeAuthJSON("openai", agent.AuthCredential{Type: "api", Key: "k"}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	infos, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(infos) < 5 {
		t.Fatalf("expected at least 5 providers, got %d", len(infos))
	}
	var openai, copilot agent.ProviderAuthInfo
	for _, p := range infos {
		switch p.ProviderID {
		case "openai":
			openai = p
		case "github-copilot":
			copilot = p
		}
	}
	if !openai.Connected {
		t.Errorf("expected openai connected after writing")
	}
	if openai.Source != agent.CredentialSourceStore {
		t.Errorf("openai Source=%q, want %q", openai.Source, agent.CredentialSourceStore)
	}
	if copilot.Connected {
		t.Errorf("expected github-copilot disconnected (not written)")
	}
	if openai.AuthType != "api" || copilot.AuthType != "device" {
		t.Errorf("unexpected auth types: openai=%s copilot=%s", openai.AuthType, copilot.AuthType)
	}
}

// TestAuthManager_FullDeviceFlow_Success drives the end-to-end happy
// path with a stub GitHub server. Verifies the goroutine walks
// pending → authorized → success, writes auth.json, and triggers the
// restart hook.
func TestAuthManager_FullDeviceFlow_Success(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)

	// First poll returns authorization_pending; second returns the
	// access token. This exercises both code paths.
	var pollCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-abc",
				"user_code":        "USER-CODE",
				"verification_uri": "https://github.com/login/device",
				"expires_in":       900,
				"interval":         1, // tight to keep the test fast
			})
		case "/login/oauth/access_token":
			n := atomic.AddInt32(&pollCount, 1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "the-token"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Redirect outbound calls to our stub server. The HTTP client's
	// transport rewrites github.com → srv.URL.
	a.httpc = &http.Client{Transport: rewriteTransport(srv.URL)}

	var restartCalls int32
	a.restart = func(context.Context) error { atomic.AddInt32(&restartCalls, 1); return nil }

	start, err := a.StartDeviceFlow(context.Background(), ProviderGitHubCopilot)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if start.UserCode != "USER-CODE" {
		t.Errorf("expected USER-CODE, got %s", start.UserCode)
	}

	// Poll status until terminal. Use a generous deadline since the
	// flow goroutine sleeps `interval+safetyMargin` between polls.
	deadline := time.Now().Add(15 * time.Second)
	var finalState agent.DeviceFlowState
	for time.Now().Before(deadline) {
		status, err := a.GetFlowStatus(context.Background(), start.FlowID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		finalState = status.State
		if status.State == agent.DeviceFlowSuccess ||
			status.State == agent.DeviceFlowError ||
			status.State == agent.DeviceFlowDenied {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if finalState != agent.DeviceFlowSuccess {
		t.Fatalf("expected success, got %s", finalState)
	}
	if got := atomic.LoadInt32(&restartCalls); got != 1 {
		t.Errorf("expected 1 restart call, got %d", got)
	}

	// auth.json should contain the token under github-copilot.
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var stored map[string]agent.AuthCredential
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode auth.json: %v", err)
	}
	if stored[ProviderGitHubCopilot].Access != "the-token" {
		t.Errorf("expected access=the-token, got %+v", stored[ProviderGitHubCopilot])
	}
	if stored[ProviderGitHubCopilot].Type != "oauth" {
		t.Errorf("expected type=oauth, got %s", stored[ProviderGitHubCopilot].Type)
	}
}

// TestAuthManager_FullDeviceFlow_AccessDenied verifies the goroutine
// surfaces denial back through the flow state.
func TestAuthManager_FullDeviceFlow_AccessDenied(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-abc",
				"user_code":        "USER-CODE",
				"verification_uri": "https://github.com/login/device",
				"expires_in":       900,
				"interval":         1,
			})
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
		}
	}))
	defer srv.Close()
	a.httpc = &http.Client{Transport: rewriteTransport(srv.URL)}

	start, err := a.StartDeviceFlow(context.Background(), ProviderGitHubCopilot)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := a.GetFlowStatus(context.Background(), start.FlowID)
		if status.State == agent.DeviceFlowDenied {
			return
		}
		if status.State == agent.DeviceFlowError ||
			status.State == agent.DeviceFlowSuccess {
			t.Fatalf("expected denied, got %s", status.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("flow did not reach denied state in time")
}

// rewriteTransport redirects any request to https://github.com/...
// to the test server URL, so the AuthManager's hardcoded GitHub
// endpoints can be intercepted without exposing a base-URL config knob.
func rewriteTransport(target string) http.RoundTripper {
	u, _ := url.Parse(target)
	return &rewriteRT{target: u}
}

type rewriteRT struct{ target *url.URL }

func (rt *rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "github.com" || strings.HasSuffix(req.URL.Host, ".github.com") {
		req = req.Clone(req.Context())
		req.URL.Scheme = rt.target.Scheme
		req.URL.Host = rt.target.Host
	}
	return http.DefaultTransport.RoundTrip(req)
}

// --- Anthropic provider tests ---

// Anthropic credentials live in their own sink, not opencode's
// auth.json — they have a separate consumer (the claude subprocess's
// env), and opencode rewrites auth.json so any unknown key would be
// clobbered. These tests pin that routing.

// Backend-scoped ListProviders must drop providers consumed by the
// other backend. Surfacing all entries to both backends confuses
// users (a claude-code session would see GitHub Copilot, an opencode
// session would see "Anthropic (Claude subscription)") and lets them
// connect a credential they can't actually use.
func TestAuthManager_ListProviders_FiltersByBackend(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	openCodeOnly, err := a.ListProviders(context.Background(), agent.BackendOpenCode)
	if err != nil {
		t.Fatalf("ListProviders(opencode): %v", err)
	}
	for _, p := range openCodeOnly {
		if p.Backend != agent.BackendOpenCode {
			t.Errorf("opencode filter leaked: %s has Backend=%q", p.ProviderID, p.Backend)
		}
		if p.ProviderID == ProviderAnthropicClaudeCode || p.ProviderID == ProviderAnthropicAPI {
			t.Errorf("anthropic provider %s in opencode list", p.ProviderID)
		}
	}

	claudeOnly, err := a.ListProviders(context.Background(), agent.BackendClaudeCode)
	if err != nil {
		t.Fatalf("ListProviders(claude-code): %v", err)
	}
	if len(claudeOnly) == 0 {
		t.Fatal("claude-code list should contain anthropic providers")
	}
	for _, p := range claudeOnly {
		if p.Backend != agent.BackendClaudeCode {
			t.Errorf("claude filter leaked: %s has Backend=%q", p.ProviderID, p.Backend)
		}
	}

	all, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders(all): %v", err)
	}
	if len(all) != len(openCodeOnly)+len(claudeOnly) {
		t.Errorf("all (%d) != opencode (%d) + claude (%d)", len(all), len(openCodeOnly), len(claudeOnly))
	}
}

func TestAuthManager_AnthropicProvidersInCatalog(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	infos, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	// Subscription path uses oauth-code (PTY-relayed setup-token);
	// console-API path stays api-type (paste a string).
	wantTypes := map[string]string{
		ProviderAnthropicClaudeCode: agent.AuthTypeOAuthCode,
		ProviderAnthropicAPI:        agent.AuthTypeAPI,
	}
	got := map[string]string{}
	for _, p := range infos {
		if want, ok := wantTypes[p.ProviderID]; ok {
			got[p.ProviderID] = p.AuthType
			if p.AuthType != want {
				t.Errorf("%s: AuthType=%q, want %q", p.ProviderID, p.AuthType, want)
			}
		}
	}
	if len(got) != len(wantTypes) {
		t.Errorf("missing anthropic providers; got %v, want %v", got, wantTypes)
	}
}

func TestAuthManager_AnthropicClaudeCode_RoutesToSinkNotAuthJSON(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)

	if err := a.writeAnthropicCredential(ProviderAnthropicClaudeCode, "sk-ant-oat01-xxxxxxxxxxxxx"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// opencode auth.json must NOT have an entry for the anthropic provider.
	authJSON := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if _, err := os.Stat(authJSON); !os.IsNotExist(err) {
		// File might exist if something else wrote it; check there's no anthropic key.
		data, _ := os.ReadFile(authJSON)
		if strings.Contains(string(data), ProviderAnthropicClaudeCode) {
			t.Errorf("anthropic credential leaked into opencode auth.json: %s", string(data))
		}
	}

	// The clank anthropic sink must contain the token under oauth_token.
	sinkPath := filepath.Join(home, ".local", "share", "clank", "anthropic.json")
	data, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatalf("read anthropic sink: %v", err)
	}
	var sink anthropicSink
	if err := json.Unmarshal(data, &sink); err != nil {
		t.Fatalf("decode sink: %v", err)
	}
	if sink.OAuthToken != "sk-ant-oat01-xxxxxxxxxxxxx" {
		t.Errorf("OAuthToken=%q, want token", sink.OAuthToken)
	}
	if sink.APIKey != "" {
		t.Errorf("APIKey should be empty, got %q", sink.APIKey)
	}

	// File mode 0o600.
	info, err := os.Stat(sinkPath)
	if err != nil {
		t.Fatalf("stat sink: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("sink perm=%o, want 0o600", perm)
	}
}

func TestAuthManager_AnthropicAPI_RoutesToSink(t *testing.T) {
	t.Parallel()
	a, home := newTestAuthManager(t)

	if err := a.writeAnthropicCredential(ProviderAnthropicAPI, "sk-ant-api03-abcdef"); err != nil {
		t.Fatalf("write: %v", err)
	}
	sinkPath := filepath.Join(home, ".local", "share", "clank", "anthropic.json")
	data, _ := os.ReadFile(sinkPath)
	var sink anthropicSink
	_ = json.Unmarshal(data, &sink)
	if sink.APIKey != "sk-ant-api03-abcdef" {
		t.Errorf("APIKey=%q", sink.APIKey)
	}
	if sink.OAuthToken != "" {
		t.Errorf("OAuthToken should be empty, got %q", sink.OAuthToken)
	}
}

// Connecting one flavor must clear the other so spawn-time env
// resolution can never set two competing vars at once.
func TestAuthManager_AnthropicWrite_ClearsOtherFlavor(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	if err := a.writeAnthropicCredential(ProviderAnthropicClaudeCode, "subscription-token"); err != nil {
		t.Fatalf("write subscription: %v", err)
	}
	if err := a.writeAnthropicCredential(ProviderAnthropicAPI, "sk-ant-api"); err != nil {
		t.Fatalf("write api key: %v", err)
	}
	sink, err := a.readAnthropicSink()
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	if sink.OAuthToken != "" {
		t.Errorf("OAuthToken should have been cleared, got %q", sink.OAuthToken)
	}
	if sink.APIKey != "sk-ant-api" {
		t.Errorf("APIKey=%q", sink.APIKey)
	}
}

func TestAuthManager_ListProviders_AnthropicConnectedState(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	if err := a.writeAnthropicCredential(ProviderAnthropicClaudeCode, "tok"); err != nil {
		t.Fatalf("write: %v", err)
	}
	infos, _ := a.ListProviders(context.Background(), "")
	var sub, api agent.ProviderAuthInfo
	for _, p := range infos {
		if p.ProviderID == ProviderAnthropicClaudeCode {
			sub = p
		}
		if p.ProviderID == ProviderAnthropicAPI {
			api = p
		}
	}
	if !sub.Connected {
		t.Errorf("anthropic-claude-code should be Connected after write")
	}
	if sub.Source != agent.CredentialSourceStore {
		t.Errorf("Source=%q, want %q for a sink-stored credential", sub.Source, agent.CredentialSourceStore)
	}
	if api.Connected {
		t.Errorf("anthropic-api should not be Connected when only subscription token is set")
	}
	if api.Source != "" {
		t.Errorf("Source=%q, want empty when disconnected", api.Source)
	}
}

// listAnthropicProviders returns the two Anthropic catalog entries
// from a full ListProviders call.
func listAnthropicProviders(t *testing.T, a *AuthManager) (sub, api agent.ProviderAuthInfo) {
	t.Helper()
	infos, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	for _, p := range infos {
		switch p.ProviderID {
		case ProviderAnthropicClaudeCode:
			sub = p
		case ProviderAnthropicAPI:
			api = p
		}
	}
	return sub, api
}

// With the fallback enabled and an empty sink, a machine-local claude
// login must surface as connected (source claude_cli) on the
// subscription provider only — and must NOT start injecting env vars:
// the spawned claude keeps resolving its own credential.
func TestAuthManager_ListProviders_ClaudeCLIFallback(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `if [ "$1" = "find-generic-password" ] || [ "$1" = "show-keychain-info" ]; then exit 0; fi; exit 1`)
	a, _ := newTestAuthManager(t)
	a.EnableClaudeCLIFallback()

	sub, api := listAnthropicProviders(t, a)
	if !sub.Connected || sub.Source != agent.CredentialSourceClaudeCLI {
		t.Errorf("sub = connected=%v source=%q, want connected via %s", sub.Connected, sub.Source, agent.CredentialSourceClaudeCLI)
	}
	if api.Connected {
		t.Errorf("anthropic-api should not report the borrowed subscription login")
	}
	if env := a.AnthropicEnv(); env != nil {
		t.Errorf("AnthropicEnv() = %v, want nil — the fallback is status-only", env)
	}
}

// The fallback is a wiring decision (local laptop provisioner). Left
// disabled — the sprite default — a machine-local claude login must
// not leak into provider status.
func TestAuthManager_ListProviders_ClaudeCLIFallbackDisabled(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 0`)
	a, _ := newTestAuthManager(t)

	sub, _ := listAnthropicProviders(t, a)
	if sub.Connected || sub.Source != "" {
		t.Errorf("sub = connected=%v source=%q, want disconnected with the fallback off", sub.Connected, sub.Source)
	}
}

// An explicit clank connection must win over the borrowed login: the
// user connected through clank, so status says store — which also
// keeps the disconnect affordance meaningful.
func TestAuthManager_ListProviders_StoreWinsOverClaudeCLI(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 0`)
	a, _ := newTestAuthManager(t)
	a.EnableClaudeCLIFallback()
	if err := a.writeAnthropicCredential(ProviderAnthropicClaudeCode, "tok"); err != nil {
		t.Fatalf("write: %v", err)
	}

	sub, _ := listAnthropicProviders(t, a)
	if !sub.Connected || sub.Source != agent.CredentialSourceStore {
		t.Errorf("sub = connected=%v source=%q, want connected via %s", sub.Connected, sub.Source, agent.CredentialSourceStore)
	}
}

// Env-borne credentials surface as connected (source env) on every
// host, per provider, and only for providers whose env var is
// actually set. Copilot must stay disconnected even with GITHUB_TOKEN
// exported: opencode would env-enable it, but generic tokens rarely
// carry Copilot entitlement (see providerEnvVars).
func TestAuthManager_ListProviders_EnvCredentials(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	a.lookupEnv = mapEnv(map[string]string{
		EnvClaudeCodeOAuthToken: "env-oauth",
		EnvAnthropicAPIKey:      "env-key",
		"GEMINI_API_KEY":        "env-gemini", // third name in google's any-of list
		"GITHUB_TOKEN":          "gho_generic",
		"OPENAI_API_KEY":        "", // set-but-empty is absence, matching opencode
	})

	infos, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	byID := make(map[string]agent.ProviderAuthInfo, len(infos))
	for _, p := range infos {
		byID[p.ProviderID] = p
	}
	for _, id := range []string{ProviderAnthropicClaudeCode, ProviderAnthropicAPI, "google"} {
		if p := byID[id]; !p.Connected || p.Source != agent.CredentialSourceEnv {
			t.Errorf("%s = connected=%v source=%q, want connected via %s", id, p.Connected, p.Source, agent.CredentialSourceEnv)
		}
	}
	for _, id := range []string{ProviderGitHubCopilot, "openai", "groq"} {
		if p := byID[id]; p.Connected || p.Source != "" {
			t.Errorf("%s = connected=%v source=%q, want disconnected", id, p.Connected, p.Source)
		}
	}
}

// The gateway-style bearer token (ANTHROPIC_AUTH_TOKEN) counts as
// env-borne auth for the API provider — it's how custom LLM gateways
// authenticate the spawned claude.
func TestAuthManager_ListProviders_EnvAuthTokenVariant(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	a.lookupEnv = mapEnv(map[string]string{EnvAnthropicAuthToken: "gw-bearer"})

	_, api := listAnthropicProviders(t, a)
	if !api.Connected || api.Source != agent.CredentialSourceEnv {
		t.Errorf("api = connected=%v source=%q, want connected via %s", api.Connected, api.Source, agent.CredentialSourceEnv)
	}
}

// Stored credentials must win over env vars — mirroring both spawn
// reality (the sink's ExtraEnv is appended after os.Environ, so it
// overrides) and opencode's own merge order (auth.json api entries
// merge over env-sourced providers).
func TestAuthManager_ListProviders_StoreWinsOverEnv(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	a.lookupEnv = mapEnv(map[string]string{
		EnvClaudeCodeOAuthToken: "env-oauth",
		"OPENAI_API_KEY":        "env-openai",
	})
	if err := a.writeAnthropicCredential(ProviderAnthropicClaudeCode, "sink-tok"); err != nil {
		t.Fatalf("write sink: %v", err)
	}
	if err := a.writeAuthJSON("openai", agent.AuthCredential{Type: "api", Key: "stored"}); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	infos, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	for _, p := range infos {
		if p.ProviderID == ProviderAnthropicClaudeCode || p.ProviderID == "openai" {
			if !p.Connected || p.Source != agent.CredentialSourceStore {
				t.Errorf("%s = connected=%v source=%q, want connected via %s", p.ProviderID, p.Connected, p.Source, agent.CredentialSourceStore)
			}
		}
	}
}

// Env beats the claude CLI keychain fallback: claude's own precedence
// uses an inherited env credential over its keychain login, so status
// says env even when both are present.
func TestAuthManager_ListProviders_EnvWinsOverClaudeCLI(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 0`)
	a, _ := newTestAuthManager(t)
	a.EnableClaudeCLIFallback()
	a.lookupEnv = mapEnv(map[string]string{EnvClaudeCodeOAuthToken: "env-oauth"})

	sub, _ := listAnthropicProviders(t, a)
	if !sub.Connected || sub.Source != agent.CredentialSourceEnv {
		t.Errorf("sub = connected=%v source=%q, want connected via %s", sub.Connected, sub.Source, agent.CredentialSourceEnv)
	}
}

// The production default reads the real process environment: a
// NewAuthManager-constructed manager (no injected lookup) must see a
// t.Setenv-exported key.
func TestAuthManager_ListProviders_EnvDefaultsToProcessEnv(t *testing.T) {
	// Not parallel: t.Setenv (HOME + env var).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GROQ_API_KEY", "from-process-env")
	a, err := NewAuthManager(nil)
	if err != nil {
		t.Fatalf("NewAuthManager: %v", err)
	}

	infos, err := a.ListProviders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	for _, p := range infos {
		if p.ProviderID == "groq" {
			if !p.Connected || p.Source != agent.CredentialSourceEnv {
				t.Errorf("groq = connected=%v source=%q, want connected via %s", p.Connected, p.Source, agent.CredentialSourceEnv)
			}
			return
		}
	}
	t.Fatal("groq not in catalog")
}

// Every env-detectable provider must exist in the catalog — a rename
// there would silently orphan the env mapping.
func TestProviderEnvVars_MatchCatalog(t *testing.T) {
	t.Parallel()
	inCatalog := make(map[string]bool, len(providerCatalog))
	for _, p := range providerCatalog {
		inCatalog[p.ProviderID] = true
	}
	for id := range providerEnvVars {
		if !inCatalog[id] {
			t.Errorf("providerEnvVars key %q has no catalog entry", id)
		}
	}
}

// The credentials-file variant of the fallback: no keychain entry, but
// ~/.claude/.credentials.json exists in the manager's home.
func TestAuthManager_ListProviders_ClaudeCLIFallback_CredentialsFile(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 44`)
	a, home := newTestAuthManager(t)
	a.EnableClaudeCLIFallback()
	writeClaudeCredentialsFile(t, home, `{"claudeAiOauth":{}}`)

	sub, _ := listAnthropicProviders(t, a)
	if !sub.Connected || sub.Source != agent.CredentialSourceClaudeCLI {
		t.Errorf("sub = connected=%v source=%q, want connected via %s", sub.Connected, sub.Source, agent.CredentialSourceClaudeCLI)
	}
}

// DeleteCredential for an anthropic provider must NOT trigger the
// opencode restart hook — anthropic creds are consumed by env vars
// on the next claude spawn, not by a running server that needs to
// reload.
func TestAuthManager_DeleteAnthropic_NoRestart(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	if err := a.writeAnthropicCredential(ProviderAnthropicClaudeCode, "tok"); err != nil {
		t.Fatalf("write: %v", err)
	}
	var restartCalls int32
	a.restart = func(context.Context) error { atomic.AddInt32(&restartCalls, 1); return nil }
	if err := a.DeleteCredential(context.Background(), ProviderAnthropicClaudeCode); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := atomic.LoadInt32(&restartCalls); got != 0 {
		t.Errorf("restart should not have been called for anthropic delete; got %d calls", got)
	}
	if tok := a.AnthropicOAuthToken(); tok != "" {
		t.Errorf("token should be cleared, got %q", tok)
	}
}

func TestAuthManager_AnthropicEnv_PrecedenceAndAbsence(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	// No credential → nil map → claude falls back to its own keychain.
	if env := a.AnthropicEnv(); env != nil {
		t.Errorf("expected nil env when no anthropic credential, got %v", env)
	}

	// Subscription token → CLAUDE_CODE_OAUTH_TOKEN.
	if err := a.writeAnthropicCredential(ProviderAnthropicClaudeCode, "sub-tok"); err != nil {
		t.Fatalf("write subscription: %v", err)
	}
	env := a.AnthropicEnv()
	if got := env["CLAUDE_CODE_OAUTH_TOKEN"]; got != "sub-tok" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN=%q, want sub-tok", got)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY should not be set when subscription token is present")
	}

	// Switch to API key → ANTHROPIC_API_KEY, subscription cleared.
	if err := a.writeAnthropicCredential(ProviderAnthropicAPI, "sk-api"); err != nil {
		t.Fatalf("write api: %v", err)
	}
	env = a.AnthropicEnv()
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-api" {
		t.Errorf("ANTHROPIC_API_KEY=%q, want sk-api", got)
	}
	if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN should not be set when only API key is present")
	}
}

// SubmitAPIKey is the public entry point exercised by the mux. Verify
// it routes anthropic API-key providers through the full happy path:
// writes to the sink, flow reaches Success, no opencode restart is
// invoked. (Subscription provider is oauth-code-typed and uses
// StartOAuthCodeFlow + SubmitAuthCode instead — covered in
// auth_anthropic_setup_token_test.go.)
func TestAuthManager_SubmitAPIKey_AnthropicReachesSuccess(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	var restartCalls int32
	a.restart = func(context.Context) error { atomic.AddInt32(&restartCalls, 1); return nil }

	flowID, err := a.SubmitAPIKey(context.Background(), ProviderAnthropicAPI, "sk-ant-api-xxxx", nil)
	if err != nil {
		t.Fatalf("SubmitAPIKey: %v", err)
	}
	// Spin briefly waiting for the goroutine to reach a terminal state.
	deadline := time.Now().Add(2 * time.Second)
	var st agent.DeviceFlowStatus
	for time.Now().Before(deadline) {
		st, err = a.GetFlowStatus(context.Background(), flowID)
		if err != nil {
			t.Fatalf("GetFlowStatus: %v", err)
		}
		if st.State == agent.DeviceFlowSuccess || st.State == agent.DeviceFlowError {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.State != agent.DeviceFlowSuccess {
		t.Fatalf("flow state=%v err=%q, want success", st.State, st.Error)
	}
	if got := atomic.LoadInt32(&restartCalls); got != 0 {
		t.Errorf("restart should not be called for anthropic submit; got %d", got)
	}
	if a.AnthropicAPIKey() != "sk-ant-api-xxxx" {
		t.Errorf("token not persisted")
	}
}
