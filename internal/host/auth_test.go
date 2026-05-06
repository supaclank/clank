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
// (so writes don't touch the real $HOME) and a no-op restart hook.
// homeDir is exposed for assertions on the on-disk auth.json.
func newTestAuthManager(t *testing.T) (*AuthManager, string) {
	t.Helper()
	dir := t.TempDir()
	a := &AuthManager{
		homeDir: dir,
		restart: func(context.Context) error { return nil },
		flows:   make(map[string]*flowState),
		httpc:   &http.Client{Timeout: 5 * time.Second},
	}
	return a, dir
}

func TestAuthManager_ListProviders_EmptyFile(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)

	infos, err := a.ListProviders(context.Background())
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

	// ListProviders should now report connected.
	infos, _ := a.ListProviders(context.Background())
	if !infos[0].Connected {
		t.Errorf("expected connected after write")
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

	infos, _ := a.ListProviders(context.Background())
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

	infos, err := a.ListProviders(context.Background())
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
