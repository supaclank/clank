package host

// AuthManager mediates AI provider authentication for the OpenCode
// instance running in this host's sandbox. It writes credentials
// directly into OpenCode's `~/.local/share/opencode/auth.json` and
// triggers a server restart so OpenCode picks up the new auth state.
//
// Credentials never travel through clank's infrastructure for OAuth
// providers — the device-flow polling happens between this process
// and the provider (e.g. github.com), with clank only mediating the
// UX (showing the user_code + verification URL to the TUI/mobile UI).
//
// Phase 1 supports only github-copilot. Phase 2 adds generic API-key
// providers; Phase 3+ adds other OAuth providers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
)

// ProviderGitHubCopilot is the OpenCode provider ID for the GitHub
// Copilot integration. Matches the value the upstream plugin emits
// at packages/opencode/src/plugin/github-copilot/copilot.ts.
const ProviderGitHubCopilot = "github-copilot"

// providerCatalog enumerates the providers this AuthManager knows
// how to authenticate. Phase 1 hardcoded only github-copilot; Phase 2
// added single-key API providers; Phase 3 adds providers that need
// extra prompts (Azure, Cloudflare). Keys match OpenCode's auth.json
// keys (provider IDs from the AI SDK package names or the upstream
// plugin's `auth.provider` field).
//
// The ordering controls how providers appear in the TUI list.
// Anthropic is intentionally omitted (per the v1 strategic-pivot
// non-goal). Bedrock and Vertex are also omitted for now: Bedrock
// uses bearer tokens that most users haven't pre-provisioned, and
// Vertex needs Application Default Credentials (a multi-line JSON
// service-account file) that doesn't fit the auth.json shape — both
// are deferrable to a later phase that adds richer credential types.
var providerCatalog = []agent.ProviderAuthInfo{
	{ProviderID: ProviderGitHubCopilot, DisplayName: "GitHub Copilot", AuthType: "device"},
	{ProviderID: "openai", DisplayName: "OpenAI", AuthType: "api"},
	{ProviderID: "google", DisplayName: "Google Gemini", AuthType: "api"},
	{ProviderID: "xai", DisplayName: "xAI (Grok)", AuthType: "api"},
	{ProviderID: "groq", DisplayName: "Groq", AuthType: "api"},
	{ProviderID: "deepseek", DisplayName: "DeepSeek", AuthType: "api"},
	{ProviderID: "mistral", DisplayName: "Mistral", AuthType: "api"},
	{ProviderID: "openrouter", DisplayName: "OpenRouter", AuthType: "api"},
	{
		ProviderID: "azure", DisplayName: "Azure OpenAI", AuthType: "api",
		Prompts: []agent.ProviderPrompt{
			{Key: "resourceName", Message: "Azure resource name", Placeholder: "e.g. my-models"},
		},
	},
	{
		ProviderID: "cloudflare-workers-ai", DisplayName: "Cloudflare Workers AI", AuthType: "api",
		Prompts: []agent.ProviderPrompt{
			{Key: "accountId", Message: "Cloudflare Account ID", Placeholder: "e.g. 1234567890abcdef1234567890abcdef"},
		},
	},
	{
		ProviderID: "cloudflare-ai-gateway", DisplayName: "Cloudflare AI Gateway", AuthType: "api",
		Prompts: []agent.ProviderPrompt{
			{Key: "accountId", Message: "Cloudflare Account ID", Placeholder: "e.g. 1234567890abcdef1234567890abcdef"},
			{Key: "gatewayId", Message: "AI Gateway ID", Placeholder: "e.g. my-gateway"},
		},
	},
}

// providerByID looks up a catalog entry by provider ID. Returns
// false if the ID is not known to this manager.
func providerByID(id string) (agent.ProviderAuthInfo, bool) {
	for _, p := range providerCatalog {
		if p.ProviderID == id {
			return p, true
		}
	}
	return agent.ProviderAuthInfo{}, false
}

// copilotClientID is OpenCode's GitHub OAuth app client_id, the same
// one the upstream plugin uses. Pinned here so the device flow we
// initiate is recognized by GitHub as opencode-style usage.
const copilotClientID = "Ov23li8tweQw6odWQebz"

// copilotPollSafetyMargin is added to the polling interval GitHub
// returns. Mirrors OAUTH_POLLING_SAFETY_MARGIN_MS in the upstream
// plugin — guards against clock skew that would otherwise have us
// hitting the access-token endpoint a hair too early.
const copilotPollSafetyMargin = 3 * time.Second

// flowTTL is how long an unconsumed flow's in-memory state lingers
// after reaching a terminal state. Long enough that a TUI poll on
// "success" can still see the result; short enough that abandoned
// flows clean up themselves.
const flowTTL = 10 * time.Minute

// flowState is the in-memory record for one in-progress device flow.
// Mutated by the flow goroutine; read by status handlers under flowMu.
type flowState struct {
	state      agent.DeviceFlowState
	errMsg     string
	cancel     context.CancelFunc
	finishedAt time.Time
}

// AuthManager owns provider authentication for one host (one
// OpenCode install). One per host.Service.
type AuthManager struct {
	homeDir string

	// restart triggers a full OpenCode server restart after a
	// credential write. Wired to OpenCodeBackendManager.RestartAllServers
	// at construction; tests inject a stub.
	restart func(ctx context.Context) error

	authMu sync.Mutex // serializes auth.json writes per host

	flowMu sync.Mutex
	flows  map[string]*flowState

	// httpc is used for both the device-flow start and the polling
	// loop. Tests can replace via SetHTTPClient. Default has a sane
	// timeout so a hung GitHub doesn't lock the goroutine forever.
	httpc *http.Client
}

// NewAuthManager constructs an AuthManager. Resolves $HOME via
// os.UserHomeDir() so the same code works on a Daytona container
// (where it's /root) and a developer's laptop.
func NewAuthManager(restart func(ctx context.Context) error) (*AuthManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("auth manager: resolve home dir: %w", err)
	}
	return &AuthManager{
		homeDir: home,
		restart: restart,
		flows:   make(map[string]*flowState),
		httpc:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// SetHTTPClient overrides the client used for outbound provider
// calls. Tests use this to stub GitHub.
func (a *AuthManager) SetHTTPClient(c *http.Client) {
	if c != nil {
		a.httpc = c
	}
}

// AuthJSONPath is where OpenCode stores credentials inside this host.
// Exposed for tests and verification probes.
func (a *AuthManager) AuthJSONPath() string {
	return filepath.Join(a.homeDir, ".local", "share", "opencode", "auth.json")
}

// ListProviders returns the providers this host knows how to
// authenticate, with their current connection state read from
// auth.json. The catalog is currently hardcoded (see providerCatalog);
// Phase 4 will replace this with a query against OpenCode's plugin
// auth methods so the list adapts as users install npm-based provider
// plugins.
func (a *AuthManager) ListProviders(_ context.Context) ([]agent.ProviderAuthInfo, error) {
	store, err := a.readAuthJSON()
	if err != nil {
		return nil, err
	}
	infos := make([]agent.ProviderAuthInfo, 0, len(providerCatalog))
	for _, p := range providerCatalog {
		p.Connected = store[p.ProviderID].Type != ""
		infos = append(infos, p)
	}
	return infos, nil
}

// ErrUnknownProvider is returned when a caller targets a provider
// this manager doesn't know how to authenticate.
var ErrUnknownProvider = errors.New("unknown auth provider")

// StartDeviceFlow begins a device-flow auth for providerID. Returns
// the user-facing fields the TUI surfaces and a flow_id for status
// polls. Spawns a background goroutine that polls the provider,
// writes auth.json on success, and triggers an OpenCode restart;
// the flow's in-memory state is updated as it transitions
// pending → authorized → success.
func (a *AuthManager) StartDeviceFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error) {
	info, ok := providerByID(providerID)
	if !ok || info.AuthType != "device" {
		return agent.DeviceFlowStart{}, ErrUnknownProvider
	}
	device, err := a.startCopilotDeviceCode(ctx)
	if err != nil {
		return agent.DeviceFlowStart{}, err
	}

	flowID := ulid.Make().String()
	flowCtx, cancel := context.WithCancel(context.Background())
	a.flowMu.Lock()
	a.flows[flowID] = &flowState{state: agent.DeviceFlowPending, cancel: cancel}
	a.flowMu.Unlock()

	go a.runCopilotFlow(flowCtx, flowID, device)

	return agent.DeviceFlowStart{
		FlowID:          flowID,
		DeviceCode:      device.DeviceCode,
		UserCode:        device.UserCode,
		VerificationURL: device.VerificationURI,
		ExpiresAt:       time.Now().Add(time.Duration(device.ExpiresIn) * time.Second),
		Interval:        device.Interval,
	}, nil
}

// ErrInvalidAPIKey is returned when SubmitAPIKey is called with a
// blank or whitespace-only key. Mux handlers map this to a 400.
var ErrInvalidAPIKey = errors.New("api key cannot be empty")

// ErrMissingPrompt is returned when a provider declares prompts in
// its catalog entry (e.g. Azure resourceName, Cloudflare accountId)
// but the caller didn't supply a value for one of them.
var ErrMissingPrompt = errors.New("required provider prompt missing")

// SubmitAPIKey stores an API key for providerID — plus any
// provider-specific metadata fields (Azure resource name, Cloudflare
// account/gateway IDs, etc.) — and triggers an OpenCode restart so
// the new credential takes effect. Returns a flow_id the client
// polls via GetFlowStatus to observe the authorized → success
// transition (the restart is the only long-running step; "pending"
// is essentially instantaneous for this flow type, but exposing it
// keeps the state machine uniform with device flows).
//
// metadata may be nil for providers that need only a key. When the
// catalog entry declares Prompts, every prompt key must be present
// in metadata with a non-blank value, or ErrMissingPrompt is
// returned before the goroutine spawns.
func (a *AuthManager) SubmitAPIKey(_ context.Context, providerID, key string, metadata map[string]string) (string, error) {
	info, ok := providerByID(providerID)
	if !ok || info.AuthType != "api" {
		return "", ErrUnknownProvider
	}
	if strings.TrimSpace(key) == "" {
		return "", ErrInvalidAPIKey
	}
	for _, p := range info.Prompts {
		if strings.TrimSpace(metadata[p.Key]) == "" {
			return "", fmt.Errorf("%w: %s", ErrMissingPrompt, p.Key)
		}
	}
	// Filter metadata to only keys this provider knows about, both
	// to drop typos from the client and to avoid persisting unrelated
	// fields to auth.json.
	cleaned := make(map[string]string, len(info.Prompts))
	for _, p := range info.Prompts {
		cleaned[p.Key] = strings.TrimSpace(metadata[p.Key])
	}

	flowID := ulid.Make().String()
	flowCtx, cancel := context.WithCancel(context.Background())
	a.flowMu.Lock()
	a.flows[flowID] = &flowState{state: agent.DeviceFlowPending, cancel: cancel}
	a.flowMu.Unlock()

	go a.runAPIKeyFlow(flowCtx, flowID, providerID, key, cleaned)
	return flowID, nil
}

// runAPIKeyFlow writes the credential, then restarts OpenCode. The
// state transitions mirror the device flow tail: pending → authorized
// (auth.json written, restart starting) → success (server healthy).
// Cancellation between authorized and success is honored; a kill
// signal at that point still leaves the credential in place but
// surfaces a canceled flow state to the TUI.
func (a *AuthManager) runAPIKeyFlow(ctx context.Context, flowID, providerID, key string, metadata map[string]string) {
	cred := agent.AuthCredential{Type: "api", Key: key}
	if len(metadata) > 0 {
		cred.Metadata = metadata
	}
	if err := a.writeAuthJSON(providerID, cred); err != nil {
		a.transition(flowID, agent.DeviceFlowError, "write auth.json: "+err.Error())
		return
	}
	a.transition(flowID, agent.DeviceFlowAuthorized, "")
	if a.restart != nil {
		if err := a.restart(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				a.transition(flowID, agent.DeviceFlowCanceled, "")
				return
			}
			a.transition(flowID, agent.DeviceFlowError, "restart opencode: "+err.Error())
			return
		}
	}
	a.transition(flowID, agent.DeviceFlowSuccess, "")
}

// GetFlowStatus returns the current state of flowID. Pure read.
// Returns ErrUnknownFlow if the flow doesn't exist (or has been
// GC'd after TTL).
func (a *AuthManager) GetFlowStatus(_ context.Context, flowID string) (agent.DeviceFlowStatus, error) {
	a.flowMu.Lock()
	defer a.flowMu.Unlock()
	f, ok := a.flows[flowID]
	if !ok {
		return agent.DeviceFlowStatus{}, ErrUnknownFlow
	}
	return agent.DeviceFlowStatus{State: f.state, Error: f.errMsg}, nil
}

// ErrUnknownFlow is returned when a status poll references a flow
// the manager has no record of.
var ErrUnknownFlow = errors.New("unknown flow id")

// CancelFlow signals the polling goroutine for flowID to stop and
// transitions the flow state to canceled. No-op if the flow has
// already reached a terminal state.
func (a *AuthManager) CancelFlow(_ context.Context, flowID string) error {
	a.flowMu.Lock()
	f, ok := a.flows[flowID]
	if !ok {
		a.flowMu.Unlock()
		return ErrUnknownFlow
	}
	if f.state == agent.DeviceFlowPending || f.state == agent.DeviceFlowAuthorized {
		f.state = agent.DeviceFlowCanceled
		f.finishedAt = time.Now()
		f.cancel()
	}
	a.flowMu.Unlock()
	return nil
}

// DeleteCredential removes providerID from auth.json and triggers an
// OpenCode restart. Used for "log out" actions.
func (a *AuthManager) DeleteCredential(ctx context.Context, providerID string) error {
	if _, ok := providerByID(providerID); !ok {
		return ErrUnknownProvider
	}
	if err := a.removeFromAuthJSON(providerID); err != nil {
		return err
	}
	if a.restart != nil {
		return a.restart(ctx)
	}
	return nil
}

// --- internal: device flow plumbing ---

type copilotDeviceCodeResp struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (a *AuthManager) startCopilotDeviceCode(ctx context.Context) (copilotDeviceCodeResp, error) {
	body := map[string]string{
		"client_id": copilotClientID,
		"scope":     "read:user",
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return copilotDeviceCodeResp{}, fmt.Errorf("marshal device-code body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/device/code", strings.NewReader(string(buf)))
	if err != nil {
		return copilotDeviceCodeResp{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	// Mirror the upstream plugin's User-Agent so GitHub treats this
	// flow identically to a vanilla `opencode auth login` invocation.
	req.Header.Set("User-Agent", "opencode/clank")

	resp, err := a.httpc.Do(req)
	if err != nil {
		return copilotDeviceCodeResp{}, fmt.Errorf("device-code request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return copilotDeviceCodeResp{}, fmt.Errorf("device-code request: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out copilotDeviceCodeResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return copilotDeviceCodeResp{}, fmt.Errorf("decode device-code response: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return copilotDeviceCodeResp{}, fmt.Errorf("device-code response missing required fields: %+v", out)
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return out, nil
}

type copilotTokenResp struct {
	AccessToken string `json:"access_token,omitempty"`
	Error       string `json:"error,omitempty"`
	Interval    int    `json:"interval,omitempty"`
}

// runCopilotFlow polls GitHub's access-token endpoint until the user
// authorizes (or the flow fails / times out). On success it writes
// auth.json, restarts OpenCode, and transitions the flow to success.
//
// Sleep cadence follows RFC 8628: respect the response's interval,
// add 5s on slow_down. We add a 3s safety margin to defend against
// clock skew, matching OpenCode's upstream plugin.
func (a *AuthManager) runCopilotFlow(ctx context.Context, flowID string, device copilotDeviceCodeResp) {
	interval := time.Duration(device.Interval)*time.Second + copilotPollSafetyMargin
	expiresAt := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)

	for {
		if time.Now().After(expiresAt) {
			a.transition(flowID, agent.DeviceFlowExpired, "device code expired before authorization")
			return
		}
		select {
		case <-ctx.Done():
			a.transition(flowID, agent.DeviceFlowCanceled, "")
			return
		case <-time.After(interval):
		}

		token, status, err := a.pollCopilotToken(ctx, device.DeviceCode)
		if err != nil {
			a.transition(flowID, agent.DeviceFlowError, err.Error())
			return
		}
		switch status {
		case "pending":
			continue
		case "slow_down":
			// RFC 8628 §3.5: add at least 5 seconds.
			interval = interval + 5*time.Second
			continue
		case "denied":
			a.transition(flowID, agent.DeviceFlowDenied, "user denied authorization")
			return
		case "expired":
			a.transition(flowID, agent.DeviceFlowExpired, "device code expired")
			return
		case "error":
			a.transition(flowID, agent.DeviceFlowError, "authorization failed")
			return
		case "success":
			cred := agent.AuthCredential{
				Type:    "oauth",
				Refresh: token,
				Access:  token,
				Expires: 0,
			}
			if err := a.writeAuthJSON(ProviderGitHubCopilot, cred); err != nil {
				a.transition(flowID, agent.DeviceFlowError, "write auth.json: "+err.Error())
				return
			}
			a.transition(flowID, agent.DeviceFlowAuthorized, "")
			if a.restart != nil {
				if err := a.restart(ctx); err != nil {
					a.transition(flowID, agent.DeviceFlowError, "restart opencode: "+err.Error())
					return
				}
			}
			a.transition(flowID, agent.DeviceFlowSuccess, "")
			return
		}
	}
}

// pollCopilotToken hits GitHub's access-token endpoint once. Returns
// (token, status, err) where status is one of: "success", "pending",
// "slow_down", "denied", "expired", "error". The caller drives the
// retry loop based on status.
func (a *AuthManager) pollCopilotToken(ctx context.Context, deviceCode string) (string, string, error) {
	body := url.Values{}
	body.Set("client_id", copilotClientID)
	body.Set("device_code", deviceCode)
	body.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(body.Encode()))
	if err != nil {
		return "", "error", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "opencode/clank")

	resp, err := a.httpc.Do(req)
	if err != nil {
		return "", "error", fmt.Errorf("token poll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "error", fmt.Errorf("token poll: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out copilotTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "error", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken != "" {
		return out.AccessToken, "success", nil
	}
	switch out.Error {
	case "authorization_pending":
		return "", "pending", nil
	case "slow_down":
		return "", "slow_down", nil
	case "access_denied":
		return "", "denied", nil
	case "expired_token":
		return "", "expired", nil
	default:
		return "", "error", nil
	}
}

// transition mutates the flow state under flowMu. Records the time
// terminal states reached so a future GC pass can prune them.
func (a *AuthManager) transition(flowID string, state agent.DeviceFlowState, errMsg string) {
	a.flowMu.Lock()
	defer a.flowMu.Unlock()
	f, ok := a.flows[flowID]
	if !ok {
		return
	}
	f.state = state
	if errMsg != "" {
		f.errMsg = errMsg
	}
	switch state {
	case agent.DeviceFlowSuccess, agent.DeviceFlowExpired,
		agent.DeviceFlowDenied, agent.DeviceFlowError, agent.DeviceFlowCanceled:
		f.finishedAt = time.Now()
	}
	a.gcFlowsLocked()
}

// gcFlowsLocked drops finished flow entries older than flowTTL.
// Must be called with flowMu held.
func (a *AuthManager) gcFlowsLocked() {
	cutoff := time.Now().Add(-flowTTL)
	for id, f := range a.flows {
		if !f.finishedAt.IsZero() && f.finishedAt.Before(cutoff) {
			delete(a.flows, id)
		}
	}
}

// --- internal: auth.json I/O ---

// readAuthJSON loads OpenCode's auth.json and returns the providerID→credential
// map. Returns an empty map if the file doesn't exist (which is the normal
// state on a fresh sandbox before any provider has been connected).
func (a *AuthManager) readAuthJSON() (map[string]agent.AuthCredential, error) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	return a.readAuthJSONLocked()
}

func (a *AuthManager) readAuthJSONLocked() (map[string]agent.AuthCredential, error) {
	path := a.AuthJSONPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]agent.AuthCredential{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read auth.json: %w", err)
	}
	if len(data) == 0 {
		return map[string]agent.AuthCredential{}, nil
	}
	var out map[string]agent.AuthCredential
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode auth.json: %w", err)
	}
	if out == nil {
		out = map[string]agent.AuthCredential{}
	}
	return out, nil
}

// writeAuthJSON merges cred into the existing auth.json under
// providerID and rewrites the file atomically. Creates parent dirs
// at 0o700 to mirror OpenCode's expectations.
func (a *AuthManager) writeAuthJSON(providerID string, cred agent.AuthCredential) error {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	store, err := a.readAuthJSONLocked()
	if err != nil {
		return err
	}
	store[providerID] = cred
	return a.persistAuthJSONLocked(store)
}

// removeFromAuthJSON deletes providerID from auth.json. No-op if
// the entry doesn't exist.
func (a *AuthManager) removeFromAuthJSON(providerID string) error {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	store, err := a.readAuthJSONLocked()
	if err != nil {
		return err
	}
	if _, ok := store[providerID]; !ok {
		return nil
	}
	delete(store, providerID)
	return a.persistAuthJSONLocked(store)
}

func (a *AuthManager) persistAuthJSONLocked(store map[string]agent.AuthCredential) error {
	path := a.AuthJSONPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create auth dir: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode auth.json: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp auth.json: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename auth.json: %w", err)
	}
	return nil
}
