package host

// AuthManager mediates AI provider authentication for agent CLIs
// running in this host's sandbox. Credentials live in two sinks:
//   - OpenCode providers → ~/.local/share/opencode/auth.json
//     (opencode's own schema; a server restart picks up changes)
//   - Anthropic providers → ~/.local/share/clank/anthropic.json
//     (our schema; the next claude-code spawn picks up changes via
//     CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY env vars)
//
// Credentials never travel through clank's infrastructure for OAuth
// providers — the device-flow polling happens between this process
// and the provider (e.g. github.com), with clank only mediating the
// UX (showing the user_code + verification URL to the TUI/mobile UI).

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

// ProviderAnthropicClaudeCode is the clank provider ID for the
// Claude.ai subscription path (Pro/Max/Team/Enterprise). The
// credential is the long-lived token printed by `claude setup-token`,
// passed to the claude subprocess as CLAUDE_CODE_OAUTH_TOKEN.
const ProviderAnthropicClaudeCode = "anthropic-claude-code"

// ProviderAnthropicAPI is the clank provider ID for pay-per-use
// console.anthropic.com API keys. Passed as ANTHROPIC_API_KEY.
const ProviderAnthropicAPI = "anthropic-api"

// providerCatalog enumerates the providers this AuthManager knows
// how to authenticate. Three classes today:
//   - device-flow OAuth (Phase 1): github-copilot
//   - single-key API providers (Phase 2): openai, google, etc.
//   - providers needing extra prompts (Phase 3): azure, cloudflare
//   - anthropic (Phase 4): subscription token + API key, both
//     surfaced as api-style paste-a-string flows but routed to a
//     separate credential sink (see writeAnthropicSink).
//
// Bedrock and Vertex are still omitted: Bedrock uses bearer tokens
// most users haven't pre-provisioned, and Vertex needs Application
// Default Credentials (a multi-line JSON service-account file) that
// doesn't fit the paste-a-string shape — both are deferrable.
var providerCatalog = []agent.ProviderAuthInfo{
	// Claude-Code-consumed providers — credential lives in clank's
	// anthropic.json and is injected into the next claude spawn as
	// CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY. No server restart.
	// Listed first so the much shorter Claude Code section doesn't get
	// pushed off-screen by the 11-entry OpenCode list below.
	{ProviderID: ProviderAnthropicClaudeCode, DisplayName: "Anthropic (Claude subscription)", AuthType: agent.AuthTypeOAuthCode, Backend: agent.BackendClaudeCode},
	{ProviderID: ProviderAnthropicAPI, DisplayName: "Anthropic (Console API key)", AuthType: agent.AuthTypeAPI, Backend: agent.BackendClaudeCode},

	// OpenCode-consumed providers — credential lives in opencode's
	// own auth.json and an OpenCode server restart picks it up.
	{ProviderID: ProviderGitHubCopilot, DisplayName: "GitHub Copilot", AuthType: agent.AuthTypeDevice, Backend: agent.BackendOpenCode},
	{ProviderID: "openai", DisplayName: "OpenAI", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode},
	{ProviderID: "google", DisplayName: "Google Gemini", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode},
	{ProviderID: "xai", DisplayName: "xAI (Grok)", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode},
	{ProviderID: "groq", DisplayName: "Groq", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode},
	{ProviderID: "deepseek", DisplayName: "DeepSeek", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode},
	{ProviderID: "mistral", DisplayName: "Mistral", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode},
	{ProviderID: "openrouter", DisplayName: "OpenRouter", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode},
	{
		ProviderID: "azure", DisplayName: "Azure OpenAI", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode,
		Prompts: []agent.ProviderPrompt{
			{Key: "resourceName", Message: "Azure resource name", Placeholder: "e.g. my-models"},
		},
	},
	{
		ProviderID: "cloudflare-workers-ai", DisplayName: "Cloudflare Workers AI", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode,
		Prompts: []agent.ProviderPrompt{
			{Key: "accountId", Message: "Cloudflare Account ID", Placeholder: "e.g. 1234567890abcdef1234567890abcdef"},
		},
	},
	{
		ProviderID: "cloudflare-ai-gateway", DisplayName: "Cloudflare AI Gateway", AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode,
		Prompts: []agent.ProviderPrompt{
			{Key: "accountId", Message: "Cloudflare Account ID", Placeholder: "e.g. 1234567890abcdef1234567890abcdef"},
			{Key: "gatewayId", Message: "AI Gateway ID", Placeholder: "e.g. my-gateway"},
		},
	},
}

// isAnthropicProvider reports whether providerID's credential lives
// in the anthropic sink rather than opencode's auth.json.
func isAnthropicProvider(providerID string) bool {
	return providerID == ProviderAnthropicClaudeCode || providerID == ProviderAnthropicAPI
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

// flowState is the in-memory record for one in-progress auth flow
// (device-flow, api-key, or oauth-code). Mutated by the flow handler
// goroutine (for device/api) or synchronously (for oauth-code's
// post-submit step); read by status handlers under flowMu.
type flowState struct {
	state      agent.DeviceFlowState
	errMsg     string
	cancel     context.CancelFunc
	finishedAt time.Time

	// setupSession is non-nil only for oauth-code flows that have
	// spawned `claude setup-token` and are waiting for the token.
	// CancelFlow tears it down via close().
	setupSession *setupTokenSession

	// done is closed by the background token awaiter when an oauth-code
	// flow reaches a terminal state. SubmitAuthCode waits on it so it can
	// return the real outcome synchronously — preserving the wire
	// contract remote clients rely on (a 2xx submit means the token was
	// captured, not merely that the code was written). Nil for non-
	// oauth-code flows.
	done chan struct{}
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

// AnthropicSinkPath is where clank stores Anthropic credentials. A
// separate file from OpenCode's auth.json because (a) opencode rewrites
// auth.json itself and would clobber any unknown keys, and (b) the
// consumer is different — claude-code reads env vars set on its
// subprocess, not a JSON file from this directory.
func (a *AuthManager) AnthropicSinkPath() string {
	return filepath.Join(a.homeDir, ".local", "share", "clank", "anthropic.json")
}

// anthropicSink is the on-disk shape at AnthropicSinkPath. Only the
// field for the most recently connected variant is populated; the
// other is cleared on connect so the env-var precedence at spawn
// time is unambiguous.
type anthropicSink struct {
	OAuthToken string `json:"oauth_token,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
}

// AnthropicOAuthToken returns the stored CLAUDE_CODE_OAUTH_TOKEN, or
// "" if none. Callers (claude-code spawn) use this as the preferred
// credential when present.
func (a *AuthManager) AnthropicOAuthToken() string {
	sink, err := a.readAnthropicSink()
	if err != nil {
		return ""
	}
	return sink.OAuthToken
}

// AnthropicAPIKey returns the stored ANTHROPIC_API_KEY, or "".
func (a *AuthManager) AnthropicAPIKey() string {
	sink, err := a.readAnthropicSink()
	if err != nil {
		return ""
	}
	return sink.APIKey
}

// AnthropicEnv returns the env vars to inject into a spawned claude
// subprocess. Subscription token wins over API key when both are set,
// which is the opposite of claude's default precedence — but in our
// model the user explicitly connected one or the other; writes clear
// the other variant. We only ever set one var, so claude's own
// precedence resolution never kicks in.
//
// Returns nil when no Anthropic provider is connected, so claude
// falls back to its own keychain/OAuth login flow.
func (a *AuthManager) AnthropicEnv() map[string]string {
	sink, err := a.readAnthropicSink()
	if err != nil || (sink.OAuthToken == "" && sink.APIKey == "") {
		return nil
	}
	if sink.OAuthToken != "" {
		return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": sink.OAuthToken}
	}
	return map[string]string{"ANTHROPIC_API_KEY": sink.APIKey}
}

// ListProviders returns the providers this host knows how to
// authenticate, with their current connection state read from the
// appropriate sink — opencode's auth.json for OpenCode providers,
// our anthropic.json for Anthropic providers. When backend is
// non-empty, the result is filtered to providers that target that
// agent CLI (so an opencode-session compose flow doesn't surface
// "Anthropic (Claude subscription)", and a claude-code-session
// flow doesn't surface "GitHub Copilot"). Empty backend returns
// the full catalog.
func (a *AuthManager) ListProviders(_ context.Context, backend agent.BackendType) ([]agent.ProviderAuthInfo, error) {
	store, err := a.readAuthJSON()
	if err != nil {
		return nil, err
	}
	sink, err := a.readAnthropicSink()
	if err != nil {
		return nil, err
	}
	infos := make([]agent.ProviderAuthInfo, 0, len(providerCatalog))
	for _, p := range providerCatalog {
		if backend != "" && p.Backend != backend {
			continue
		}
		switch p.ProviderID {
		case ProviderAnthropicClaudeCode:
			p.Connected = sink.OAuthToken != ""
		case ProviderAnthropicAPI:
			p.Connected = sink.APIKey != ""
		default:
			p.Connected = store[p.ProviderID].Type != ""
		}
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
	if !ok || info.AuthType != agent.AuthTypeDevice {
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
	if !ok || info.AuthType != agent.AuthTypeAPI {
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

// runAPIKeyFlow writes the credential to the appropriate sink and,
// for OpenCode providers, restarts OpenCode so it picks up the new
// auth state. Anthropic providers don't restart anything — the next
// claude-code spawn inherits the new env var, existing sessions
// continue with whatever credential they started with.
//
// State transitions mirror the device flow tail: pending → authorized
// (credential persisted, restart starting if applicable) → success.
// Cancellation between authorized and success is honored; a kill
// signal at that point still leaves the credential in place but
// surfaces a canceled flow state to the TUI.
func (a *AuthManager) runAPIKeyFlow(ctx context.Context, flowID, providerID, key string, metadata map[string]string) {
	if isAnthropicProvider(providerID) {
		if err := a.writeAnthropicCredential(providerID, key); err != nil {
			a.transition(flowID, agent.DeviceFlowError, "write anthropic credential: "+err.Error())
			return
		}
		a.transition(flowID, agent.DeviceFlowAuthorized, "")
		a.transition(flowID, agent.DeviceFlowSuccess, "")
		return
	}
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

// setupTokenURLTimeout caps how long we wait for `claude setup-token`
// to print its authorize URL. Generous enough that the CLI's startup
// banner + animations complete on a cold sprite; short enough that a
// hung CLI doesn't leave a flow stuck.
const setupTokenURLTimeout = 30 * time.Second

// setupTokenAwaitTokenTimeout caps how long the background awaiter waits
// for the long-lived token to appear. It spans the human step of
// authenticating in the browser, so it's minutes, not seconds. Covers
// both completion paths: the native-local case where setup-token's own
// localhost callback finishes the flow with no paste, and the remote
// case where the token only appears after the user pastes a code.
const setupTokenAwaitTokenTimeout = 5 * time.Minute

// ErrInvalidAuthCode is returned when SubmitAuthCode is called with
// a blank code (after trimming).
var ErrInvalidAuthCode = errors.New("auth code cannot be empty")

// ErrFlowNotOAuthCode is returned when SubmitAuthCode targets a flow
// that wasn't started via StartOAuthCodeFlow.
var ErrFlowNotOAuthCode = errors.New("flow is not an oauth-code flow")

// StartOAuthCodeFlow spawns `claude setup-token` in a PTY, waits for it
// to print its authorize URL, and returns the URL + a flow_id. A
// background awaiter then captures the long-lived token from whichever
// source produces it:
//
//   - native-local: setup-token opens the user's browser and completes
//     via its OWN localhost callback — the token appears with no pasted
//     code, so the flow reaches success without SubmitAuthCode.
//   - remote: the IdP shows a code on its hosted page; the user pastes
//     it via SubmitAuthCode, and the same awaiter catches the resulting
//     token.
//
// The session lives until the awaiter finishes (success/error/timeout)
// or CancelFlow cancels it.
func (a *AuthManager) StartOAuthCodeFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error) {
	info, ok := providerByID(providerID)
	if !ok || info.AuthType != agent.AuthTypeOAuthCode {
		return agent.DeviceFlowStart{}, ErrUnknownProvider
	}

	sess, err := startSetupToken(ctx)
	if err != nil {
		return agent.DeviceFlowStart{}, fmt.Errorf("start setup-token: %w", err)
	}

	urlCtx, cancel := context.WithTimeout(ctx, setupTokenURLTimeout)
	defer cancel()
	verificationURL, err := sess.awaitURL(urlCtx)
	if err != nil {
		sess.close()
		return agent.DeviceFlowStart{}, fmt.Errorf("await authorize URL: %w", err)
	}

	// Independent lifetime: the token can arrive minutes later (after the
	// user signs in), well past the request ctx's deadline.
	awaitCtx, cancelAwait := context.WithCancel(context.Background())
	done := make(chan struct{})
	flowID := ulid.Make().String()
	a.flowMu.Lock()
	a.flows[flowID] = &flowState{
		state:        agent.DeviceFlowPending,
		cancel:       cancelAwait,
		setupSession: sess,
		done:         done,
	}
	a.flowMu.Unlock()

	go a.awaitOAuthCodeToken(awaitCtx, flowID, providerID, sess, done)

	return agent.DeviceFlowStart{
		FlowID:          flowID,
		VerificationURL: verificationURL,
		ExpiresAt:       time.Now().Add(setupTokenAwaitTokenTimeout),
	}, nil
}

// awaitOAuthCodeToken blocks until setup-token prints the long-lived
// token (from either completion path), persists it, and transitions the
// flow to success. Owns session teardown on every exit path and closes
// done so a waiting SubmitAuthCode observes the terminal state. A failure
// or timeout transitions the flow to error — unless CancelFlow already
// moved it to a terminal state, which failFlowIfActive leaves intact.
func (a *AuthManager) awaitOAuthCodeToken(ctx context.Context, flowID, providerID string, sess *setupTokenSession, done chan struct{}) {
	defer close(done)
	defer sess.close()

	tokCtx, cancel := context.WithTimeout(ctx, setupTokenAwaitTokenTimeout)
	defer cancel()
	token, err := sess.awaitToken(tokCtx)
	if err != nil {
		a.failFlowIfActive(flowID, "await token: "+err.Error())
		return
	}
	if err := a.writeAnthropicCredential(providerID, token); err != nil {
		a.failFlowIfActive(flowID, "write anthropic credential: "+err.Error())
		return
	}
	// Drop the session reference before the success transition so a
	// racing SubmitAuthCode sees the flow is done rather than writing to
	// a closing PTY.
	a.flowMu.Lock()
	if cur, ok := a.flows[flowID]; ok {
		cur.setupSession = nil
	}
	a.flowMu.Unlock()
	a.transition(flowID, agent.DeviceFlowSuccess, "")
}

// failFlowIfActive transitions the flow to error only while it's still
// non-terminal. Guards against a late awaiter error (e.g. ctx canceled
// by CancelFlow) clobbering a Canceled/Success state already recorded.
func (a *AuthManager) failFlowIfActive(flowID, errMsg string) {
	a.flowMu.Lock()
	defer a.flowMu.Unlock()
	f, ok := a.flows[flowID]
	if !ok || (f.state != agent.DeviceFlowPending && f.state != agent.DeviceFlowAuthorized) {
		return
	}
	f.state = agent.DeviceFlowError
	f.errMsg = errMsg
	f.finishedAt = time.Now()
	a.gcFlowsLocked()
}

// SubmitAuthCode writes a user-pasted code into the running setup-token
// subprocess, then blocks until the background token awaiter (started in
// StartOAuthCodeFlow) captures the token and reaches a terminal state.
// It returns nil on success and the flow's error otherwise, preserving
// the synchronous contract remote clients rely on. Only the remote path
// calls this; the native-local path self-completes via the browser
// callback and never needs a pasted code.
func (a *AuthManager) SubmitAuthCode(ctx context.Context, providerID, flowID, code string) error {
	info, ok := providerByID(providerID)
	if !ok || info.AuthType != agent.AuthTypeOAuthCode {
		return ErrUnknownProvider
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return ErrInvalidAuthCode
	}

	a.flowMu.Lock()
	f, ok := a.flows[flowID]
	if !ok {
		a.flowMu.Unlock()
		return ErrUnknownFlow
	}
	sess := f.setupSession
	done := f.done
	a.flowMu.Unlock()
	if sess == nil {
		// No live session: the flow already reached a terminal state
		// (commonly a local self-complete that beat the paste). The
		// caller's status poll already reflects the real outcome.
		return ErrFlowNotOAuthCode
	}

	// On a write failure the subprocess has almost certainly exited; the
	// awaiter catches that via doneCh and drives the flow to error.
	if err := sess.submitCode(code); err != nil {
		return fmt.Errorf("submit code: %w", err)
	}

	// Block until the awaiter records the terminal outcome, so the
	// response reflects whether the token was actually captured.
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	st, err := a.GetFlowStatus(context.Background(), flowID)
	if err != nil {
		return err
	}
	if st.State != agent.DeviceFlowSuccess {
		if st.Error != "" {
			return errors.New(st.Error)
		}
		return fmt.Errorf("oauth-code flow ended in state %q", st.State)
	}
	return nil
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

// DeleteCredential removes providerID's credential from the appropriate
// sink. For OpenCode providers, triggers a server restart so the new
// auth state takes effect; for Anthropic providers, no restart — the
// next claude-code spawn simply sees no env var.
func (a *AuthManager) DeleteCredential(ctx context.Context, providerID string) error {
	if _, ok := providerByID(providerID); !ok {
		return ErrUnknownProvider
	}
	if isAnthropicProvider(providerID) {
		return a.removeAnthropicCredential(providerID)
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

// --- internal: anthropic sink I/O ---

func (a *AuthManager) readAnthropicSink() (anthropicSink, error) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	return a.readAnthropicSinkLocked()
}

func (a *AuthManager) readAnthropicSinkLocked() (anthropicSink, error) {
	path := a.AnthropicSinkPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
		return anthropicSink{}, nil
	}
	if err != nil {
		return anthropicSink{}, fmt.Errorf("read anthropic sink: %w", err)
	}
	var out anthropicSink
	if err := json.Unmarshal(data, &out); err != nil {
		return anthropicSink{}, fmt.Errorf("decode anthropic sink: %w", err)
	}
	return out, nil
}

// writeAnthropicCredential persists key under the field that matches
// providerID. Writes only one variant at a time — connecting one
// flavor clears the other so spawn-time precedence is unambiguous.
func (a *AuthManager) writeAnthropicCredential(providerID, key string) error {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	var sink anthropicSink
	switch providerID {
	case ProviderAnthropicClaudeCode:
		sink.OAuthToken = key
	case ProviderAnthropicAPI:
		sink.APIKey = key
	default:
		return fmt.Errorf("writeAnthropicCredential: not an anthropic provider: %s", providerID)
	}
	return a.persistAnthropicSinkLocked(sink)
}

// removeAnthropicCredential clears the field for providerID. Leaves
// the other flavor intact so a user logged into both (unlikely but
// possible across versions) doesn't lose the other one by accident.
func (a *AuthManager) removeAnthropicCredential(providerID string) error {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	sink, err := a.readAnthropicSinkLocked()
	if err != nil {
		return err
	}
	switch providerID {
	case ProviderAnthropicClaudeCode:
		sink.OAuthToken = ""
	case ProviderAnthropicAPI:
		sink.APIKey = ""
	default:
		return fmt.Errorf("removeAnthropicCredential: not an anthropic provider: %s", providerID)
	}
	if sink.OAuthToken == "" && sink.APIKey == "" {
		// Both cleared — delete the file instead of leaving an empty JSON.
		path := a.AnthropicSinkPath()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove anthropic sink: %w", err)
		}
		return nil
	}
	return a.persistAnthropicSinkLocked(sink)
}

func (a *AuthManager) persistAnthropicSinkLocked(sink anthropicSink) error {
	path := a.AnthropicSinkPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create anthropic sink dir: %w", err)
	}
	data, err := json.MarshalIndent(sink, "", "  ")
	if err != nil {
		return fmt.Errorf("encode anthropic sink: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp anthropic sink: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename anthropic sink: %w", err)
	}
	return nil
}
