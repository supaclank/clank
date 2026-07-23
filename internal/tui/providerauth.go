package tui

// providerauth.go — modal flow for connecting an AI provider on the
// active host. Modeled after the themepicker / modelpicker pattern:
// a single tea.Model with an internal phase field that walks the
// user through:
//
//   list → confirm → (deviceShow|apikeyEntry) → awaiting → success | error
//
// The phase after `confirm` depends on the selected provider's
// AuthType: "device" providers (Phase 1: GitHub Copilot) jump
// straight into the awaiting phase with the URL+user_code displayed
// alongside the polling spinner; "api" providers (Phase 2: OpenAI,
// Google, xAI, Groq, DeepSeek, Mistral, OpenRouter) collect a
// pasted key in apikeyEntry and then transition to a stripped-down
// awaiting phase that only renders the restart spinner.
//
// All polling/start calls go through the hub. The hub forwards to
// clank-host in the sandbox; nothing in the TUI talks to providers
// directly.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// providerAuthCaller is the call surface the modal needs to drive an
// auth flow against a host. Two implementations exist today:
//   - daemonclient.HostClient via hub.Host(hostname), used by the
//     Settings entry to target the local clank-host through the hub.
//   - cloud.AuthCaller, used by the Cloud panel's
//     "Connect provider (in sandbox)" entry to talk directly to the
//     active remote gateway with the user's OAuth bearer.
//
// Mirrors the names on daemonclient.HostClient so existing call sites
// satisfy the interface without changes.
type providerAuthCaller interface {
	ListAuthProviders(ctx context.Context, backend agent.BackendType) ([]agent.ProviderAuthInfo, error)
	StartAuthDeviceFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error)
	SubmitAuthAPIKey(ctx context.Context, providerID, key string, metadata map[string]string) (agent.DeviceFlowStart, error)
	StartAuthOAuthCodeFlow(ctx context.Context, providerID string) (agent.DeviceFlowStart, error)
	SubmitAuthCode(ctx context.Context, providerID, flowID, code string) error
	AuthFlowStatus(ctx context.Context, providerID, flowID string) (agent.DeviceFlowStatus, error)
	CancelAuthFlow(ctx context.Context, providerID, flowID string) error
}

// providerAuthCancelMsg signals the inbox to dismiss the modal.
type providerAuthCancelMsg struct{}

// providerAuthDoneMsg signals the inbox the flow finished
// successfully (any subsequent message would be informational only).
type providerAuthDoneMsg struct{}

// Internal messages: each is the result of a tea.Cmd. The model
// processes them in Update to advance phase state.
type providerListLoadedMsg struct {
	providers []agent.ProviderAuthInfo
	err       error
}

type providerStartedMsg struct {
	start agent.DeviceFlowStart
	err   error
}

type providerPollTickMsg struct{}

type providerStatusMsg struct {
	status agent.DeviceFlowStatus
	err    error
}

type providerAuthPhase int

const (
	providerPhaseLoading providerAuthPhase = iota
	providerPhaseList
	providerPhaseConfirm
	// providerPhaseAPIKey collects a pasted API key for "api"
	// providers. Skipped for "device" and "oauth-code" providers.
	providerPhaseAPIKey
	// providerPhaseOAuthCode handles oauth-code providers (Anthropic
	// Claude subscription). It shows the authorize URL printed by the
	// host's PTY-relayed `claude setup-token` plus an optional textinput
	// for a pasted code, and polls flow status the whole time. A
	// native-local flow self-completes via setup-token's own browser
	// callback (no paste) and transitions straight to success; a remote
	// flow needs the user to paste the code the IdP shows.
	providerPhaseOAuthCode
	// providerPhaseAwaiting covers both "waiting for the user to
	// authorize in their browser" (device) and "waiting for the
	// OpenCode server to come back up" (api-key) and "exchanging the
	// auth code for a token" (oauth-code). Polling starts the moment
	// we transition into this phase — no enter press required.
	providerPhaseAwaiting
	providerPhaseSuccess
	providerPhaseError
)

const providerAuthPollInterval = 2 * time.Second

// providerAuthModel is the modal's state. Constructed via
// newProviderAuthModel; rendered through overlayCenter by the inbox.
type providerAuthModel struct {
	caller providerAuthCaller

	// backend, when non-empty, scopes the provider list to those
	// consumed by that agent CLI (opencode | claude-code). The model
	// picker passes its current backend through so a claude-code
	// session's "+ Connect provider…" shows only Anthropic entries
	// and an opencode session shows only the opencode-managed ones.
	// Empty = no filter (host-scoped entries into the modal show both
	// backends, grouped under section headers in the list view).
	backend agent.BackendType

	phase providerAuthPhase

	providers []agent.ProviderAuthInfo
	cursor    int

	// activeProvider is the entry the user is connecting; populated
	// once they hit Enter on the list.
	activeProvider agent.ProviderAuthInfo

	// flow holds the start payload returned by /device/start or
	// /apikey; UserCode/VerificationURL are populated only for
	// device flows.
	flow agent.DeviceFlowStart

	// flowState tracks the most recent status read; used to drive
	// the awaiting phase's spinner label.
	flowState agent.DeviceFlowState

	// apiKey is the textinput model used during providerPhaseAPIKey.
	apiKey textinput.Model

	// promptIndex tracks which prompt input the user is currently
	// filling, for providers whose catalog entry has Prompts.
	// Range: 0 ..= len(activeProvider.Prompts). When equal to the
	// length, the user is on the API key itself.
	promptIndex int

	// metadata accumulates the prompt answers as the user advances
	// through them. Submitted as the request body's "metadata" field.
	metadata map[string]string

	errMsg  string
	spinner spinner.Model

	// slowLoadHint, when non-empty, is rendered under the loading
	// spinner once we've been in providerPhaseLoading for more than
	// providerSlowLoadAfter. Used by the Cloud entry to set
	// expectations on a cold-start sprite; the Settings entry leaves
	// it empty because the local host is always warm.
	slowLoadHint string

	// loadingStartedAt is the wall-clock time we entered the initial
	// loading phase. Used together with slowLoadHint to decide when to
	// show the cold-start nudge.
	loadingStartedAt time.Time
}

// providerSlowLoadAfter is how long providerPhaseLoading can sit
// without finishing before we show the slowLoadHint (when set).
const providerSlowLoadAfter = 2 * time.Second

// providerListLoadTimeout caps the GET /auth/providers call. The
// gateway can take 10–20 seconds to wake a cold sprite, so the
// previous 5-second cap fired prematurely on first use of the cloud
// entry. Local calls return in milliseconds, so the larger ceiling
// has no downside there.
const providerListLoadTimeout = 30 * time.Second

func newProviderAuthModel(caller providerAuthCaller, backend agent.BackendType, slowLoadHint string) providerAuthModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(primaryColor)

	ti := textinput.New()
	ti.Placeholder = "sk-..."
	ti.CharLimit = 256
	ti.Prompt = "› "
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(primaryColor)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)
	ti.SetWidth(48)

	return providerAuthModel{
		caller:           caller,
		backend:          backend,
		phase:            providerPhaseLoading,
		spinner:          sp,
		apiKey:           ti,
		slowLoadHint:     slowLoadHint,
		loadingStartedAt: time.Now(),
	}
}

func (m providerAuthModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.loadProvidersCmd())
}

// Update is the central state-machine dispatcher. Most cases mutate
// the model and emit a follow-up tea.Cmd; phase transitions all flow
// through here.
func (m providerAuthModel) Update(msg tea.Msg) (providerAuthModel, tea.Cmd) {
	switch msg := msg.(type) {
	case providerListLoadedMsg:
		if msg.err != nil {
			m.phase = providerPhaseError
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.providers = msg.providers
		m.phase = providerPhaseList
		if m.cursor >= len(m.providers) {
			m.cursor = 0
		}
		return m, nil

	case providerStartedMsg:
		if msg.err != nil {
			m.phase = providerPhaseError
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.flow = msg.start
		m.flowState = agent.DeviceFlowPending
		// oauth-code flows show the URL + an optional paste field, but
		// poll from the start: a native-local flow self-completes via
		// setup-token's own browser callback, so success can arrive
		// without any pasted code. device + api-key flows go straight to
		// awaiting + polling.
		if m.activeProvider.AuthType == agent.AuthTypeOAuthCode {
			m.phase = providerPhaseOAuthCode
			m.configureInputForOAuthCode()
			return m, tea.Batch(m.apiKey.Focus(), m.statusCmd())
		}
		m.phase = providerPhaseAwaiting
		return m, m.statusCmd()

	case providerPollTickMsg:
		// oauth-code polls during its own phase too, so a self-completing
		// local flow is detected before the user touches the paste field.
		if m.phase != providerPhaseAwaiting && m.phase != providerPhaseOAuthCode {
			return m, nil
		}
		return m, m.statusCmd()

	case providerStatusMsg:
		if msg.err != nil {
			m.phase = providerPhaseError
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.flowState = msg.status.State
		switch msg.status.State {
		case agent.DeviceFlowSuccess:
			m.phase = providerPhaseSuccess
			return m, nil
		case agent.DeviceFlowError, agent.DeviceFlowDenied,
			agent.DeviceFlowExpired, agent.DeviceFlowCanceled:
			m.phase = providerPhaseError
			if msg.status.Error != "" {
				m.errMsg = msg.status.Error
			} else {
				m.errMsg = string(msg.status.State)
			}
			return m, nil
		default:
			// pending / authorized: keep polling.
			return m, m.pollTickCmd()
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	// Anything else: forward to the textinput when a phase is using it.
	if m.phase == providerPhaseAPIKey || m.phase == providerPhaseOAuthCode {
		var cmd tea.Cmd
		m.apiKey, cmd = m.apiKey.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m providerAuthModel) handleKey(msg tea.KeyPressMsg) (providerAuthModel, tea.Cmd) {
	msg = normalizeKeyCase(msg)
	cancel := key.Matches(msg, key.NewBinding(key.WithKeys("esc")))

	switch m.phase {
	case providerPhaseLoading:
		if cancel {
			return m, func() tea.Msg { return providerAuthCancelMsg{} }
		}

	case providerPhaseList:
		if cancel {
			return m, func() tea.Msg { return providerAuthCancelMsg{} }
		}
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
			if m.cursor > 0 {
				m.cursor--
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
			if m.cursor < len(m.providers)-1 {
				m.cursor++
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("shift+up"))):
			// Jump to the previous backend section's first row. Mirrors
			// the sidebar / inbox pattern via the shared breakpoints
			// helper.
			if bp := providerSectionBreakpoints(m.providers); len(bp) > 0 {
				m.cursor = prevBreakpoint(bp, m.cursor)
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("shift+down"))):
			// Jump to the next backend section's first row.
			if bp := providerSectionBreakpoints(m.providers); len(bp) > 0 {
				m.cursor = nextBreakpoint(bp, m.cursor)
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			if m.cursor >= 0 && m.cursor < len(m.providers) {
				m.activeProvider = m.providers[m.cursor]
				m.phase = providerPhaseConfirm
			}
		}

	case providerPhaseConfirm:
		if cancel {
			return m, func() tea.Msg { return providerAuthCancelMsg{} }
		}
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("y", "Y", "enter"))):
			// Branch by auth type. Device + oauth-code providers go
			// straight to a /start call (we need the IdP-issued URL
			// before showing anything to the user); API-key providers
			// collect input first.
			switch m.activeProvider.AuthType {
			case agent.AuthTypeDevice:
				return m, m.startFlowCmd(m.activeProvider.ProviderID)
			case agent.AuthTypeOAuthCode:
				return m, m.startOAuthCodeFlowCmd(m.activeProvider.ProviderID)
			case agent.AuthTypeAPI:
				m.phase = providerPhaseAPIKey
				m.promptIndex = 0
				m.metadata = make(map[string]string, len(m.activeProvider.Prompts))
				m.configureInputForCurrentField()
				return m, m.apiKey.Focus()
			default:
				m.phase = providerPhaseError
				m.errMsg = "unsupported auth type: " + m.activeProvider.AuthType
				return m, nil
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("n", "N"))):
			m.phase = providerPhaseList
			return m, nil
		}

	case providerPhaseAPIKey:
		if cancel {
			return m, func() tea.Msg { return providerAuthCancelMsg{} }
		}
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			val := strings.TrimSpace(m.apiKey.Value())
			if val == "" {
				m.errMsg = "value cannot be empty"
				return m, nil
			}
			m.errMsg = ""
			// Are we still collecting metadata prompts, or on the
			// final API key field?
			if m.promptIndex < len(m.activeProvider.Prompts) {
				field := m.activeProvider.Prompts[m.promptIndex]
				m.metadata[field.Key] = val
				m.promptIndex++
				m.configureInputForCurrentField()
				return m, m.apiKey.Focus()
			}
			// All fields collected — submit.
			return m, m.submitAPIKeyCmd(m.activeProvider.ProviderID, val, m.metadata)
		}
		// Forward any other key to the textinput.
		var cmd tea.Cmd
		m.apiKey, cmd = m.apiKey.Update(msg)
		return m, cmd

	case providerPhaseOAuthCode:
		if cancel {
			return m, m.cancelFlowCmd()
		}
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			val := strings.TrimSpace(m.apiKey.Value())
			if val == "" {
				m.errMsg = "code cannot be empty"
				return m, nil
			}
			m.errMsg = ""
			// Move to awaiting and fire the synchronous submit; its
			// result message flips us to success or error.
			m.phase = providerPhaseAwaiting
			m.flowState = agent.DeviceFlowPending
			return m, m.submitAuthCodeCmd(m.activeProvider.ProviderID, m.flow.FlowID, val)
		}
		var cmd tea.Cmd
		m.apiKey, cmd = m.apiKey.Update(msg)
		return m, cmd

	case providerPhaseAwaiting:
		if cancel {
			return m, m.cancelFlowCmd()
		}

	case providerPhaseSuccess:
		// Any key dismisses.
		return m, func() tea.Msg { return providerAuthDoneMsg{} }

	case providerPhaseError:
		// Any key dismisses.
		return m, func() tea.Msg { return providerAuthCancelMsg{} }
	}

	return m, nil
}

// --- tea.Cmd helpers ---

func (m providerAuthModel) loadProvidersCmd() tea.Cmd {
	caller := m.caller
	backend := m.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), providerListLoadTimeout)
		defer cancel()
		providers, err := caller.ListAuthProviders(ctx, backend)
		return providerListLoadedMsg{providers: providers, err: err}
	}
}

func (m providerAuthModel) startFlowCmd(providerID string) tea.Cmd {
	caller := m.caller
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start, err := caller.StartAuthDeviceFlow(ctx, providerID)
		return providerStartedMsg{start: start, err: err}
	}
}

func (m providerAuthModel) submitAPIKeyCmd(providerID, key string, metadata map[string]string) tea.Cmd {
	caller := m.caller
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start, err := caller.SubmitAuthAPIKey(ctx, providerID, key, metadata)
		return providerStartedMsg{start: start, err: err}
	}
}

// startOAuthCodeFlowCmd asks the host to spawn `claude setup-token`
// and return the authorize URL it prints. Reuses providerStartedMsg
// — the start payload carries FlowID + VerificationURL (UserCode is
// empty, since this flow has no shown user_code).
func (m providerAuthModel) startOAuthCodeFlowCmd(providerID string) tea.Cmd {
	caller := m.caller
	return func() tea.Msg {
		// Generous timeout — the host's PTY-spawn + URL extraction
		// can take a few seconds on a cold sprite while the CLI's
		// startup banner animates.
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		start, err := caller.StartAuthOAuthCodeFlow(ctx, providerID)
		return providerStartedMsg{start: start, err: err}
	}
}

// submitAuthCodeCmd delivers the user-pasted code to the host and blocks
// until the background awaiter reaches a terminal state. SubmitAuthCode
// handles the self-complete case (sess already nil) by waiting on done
// and returning nil, so this correctly drives the UI to success/error
// without needing a separate poll tick.
func (m providerAuthModel) submitAuthCodeCmd(providerID, flowID, code string) tea.Cmd {
	caller := m.caller
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := caller.SubmitAuthCode(ctx, providerID, flowID, code); err != nil {
			return providerStatusMsg{status: agent.DeviceFlowStatus{
				State: agent.DeviceFlowError,
				Error: err.Error(),
			}}
		}
		return providerStatusMsg{status: agent.DeviceFlowStatus{State: agent.DeviceFlowSuccess}}
	}
}

// configureInputForOAuthCode swaps the textinput to its "paste a
// code" configuration. Echo on — codes shown on Anthropic's redirect
// page are short alphanumeric strings, not secrets in the same way
// an API key is, and seeing what you typed reduces paste errors.
func (m *providerAuthModel) configureInputForOAuthCode() {
	m.apiKey.SetValue("")
	m.apiKey.Placeholder = "paste code from your browser"
	m.apiKey.EchoMode = textinput.EchoNormal
}

// configureInputForCurrentField re-skins the textinput to match
// whichever field the user is on — masking turns on only for the
// final API key (so accountId / resourceName / etc. are typed
// visibly), and the placeholder mirrors the catalog's prompt hint.
func (m *providerAuthModel) configureInputForCurrentField() {
	m.apiKey.SetValue("")
	if m.promptIndex < len(m.activeProvider.Prompts) {
		p := m.activeProvider.Prompts[m.promptIndex]
		m.apiKey.Placeholder = p.Placeholder
		m.apiKey.EchoMode = textinput.EchoNormal
	} else {
		m.apiKey.Placeholder = "sk-..."
		m.apiKey.EchoMode = textinput.EchoPassword
	}
}

func (m providerAuthModel) statusCmd() tea.Cmd {
	caller := m.caller
	provider := m.activeProvider.ProviderID
	flowID := m.flow.FlowID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		status, err := caller.AuthFlowStatus(ctx, provider, flowID)
		return providerStatusMsg{status: status, err: err}
	}
}

func (m providerAuthModel) pollTickCmd() tea.Cmd {
	return tea.Tick(providerAuthPollInterval, func(time.Time) tea.Msg {
		return providerPollTickMsg{}
	})
}

func (m providerAuthModel) cancelFlowCmd() tea.Cmd {
	caller := m.caller
	provider := m.activeProvider.ProviderID
	flowID := m.flow.FlowID
	return func() tea.Msg {
		if flowID == "" {
			return providerAuthCancelMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = caller.CancelAuthFlow(ctx, provider, flowID)
		return providerAuthCancelMsg{}
	}
}

// --- View ---

func (m providerAuthModel) View() string {
	const menuWidth = 60
	innerWidth := menuWidth - 4

	var sb strings.Builder

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(textColor).
		Width(innerWidth).
		Render("Connect Provider")
	sb.WriteString(title)
	sb.WriteString("\n\n")

	switch m.phase {
	case providerPhaseLoading:
		sb.WriteString(m.spinner.View())
		sb.WriteString(" Loading providers…")
		if m.slowLoadHint != "" && time.Since(m.loadingStartedAt) >= providerSlowLoadAfter {
			sb.WriteString("\n\n")
			sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).Italic(true).
				Render(m.slowLoadHint))
		}

	case providerPhaseList:
		if len(m.providers) == 0 {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Render("  no providers available"))
		} else {
			// When the modal was opened without a backend filter (host-
			// scoped entries from Settings / Cloud), render section
			// headers grouping providers by their Backend field. The
			// session-compose entry passes a single backend and gets a
			// homogeneous list with no headers.
			groupByBackend := m.backend == ""
			var lastBackend agent.BackendType
			for i, p := range m.providers {
				if groupByBackend && (i == 0 || p.Backend != lastBackend) {
					sb.WriteString(renderBackendSectionHeader(p.Backend, innerWidth))
					sb.WriteString("\n")
					lastBackend = p.Backend
				}
				prefix := "  "
				labelStyle := lipgloss.NewStyle().Foreground(textColor)
				if i == m.cursor {
					prefix = "▶ "
					labelStyle = labelStyle.Bold(true).Background(primaryColor)
				}
				status := lipgloss.NewStyle().Foreground(dimColor).Render("not connected")
				if p.Connected {
					label := "connected"
					// Borrowed credentials (machine's claude login, env
					// vars) aren't disconnectable through clank — say
					// where they're from.
					switch p.Source {
					case agent.CredentialSourceClaudeCLI:
						label = "connected (claude cli)"
					case agent.CredentialSourceEnv:
						label = "connected (env)"
					}
					status = lipgloss.NewStyle().Foreground(successColor).Render(label)
				}
				row := fmt.Sprintf("%s%s", prefix, labelStyle.Render(p.DisplayName))
				gap := innerWidth - lipgloss.Width(row) - lipgloss.Width(status) - 2
				if gap < 1 {
					gap = 1
				}
				sb.WriteString(row + strings.Repeat(" ", gap) + status)
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
		hint := "↑↓ navigate · enter select · esc cancel"
		if len(providerSectionBreakpoints(m.providers)) > 1 {
			hint = "↑↓ navigate · shift+↑↓ jump section · enter select · esc cancel"
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Render(hint))

	case providerPhaseConfirm:
		var warn string
		if isAnthropicProviderID(m.activeProvider.ProviderID) {
			// Anthropic creds are env vars consumed by the NEXT claude
			// spawn — no in-place restart, no impact on running sessions.
			warn = fmt.Sprintf(
				"Connecting %s stores credentials for future claude-code sessions.\n"+
					"Sessions already running continue with their current credentials.",
				m.activeProvider.DisplayName,
			)
		} else {
			warn = fmt.Sprintf(
				"Connecting %s will restart the OpenCode server in this sandbox.\n"+
					"Any sessions currently running will need to be restarted manually.",
				m.activeProvider.DisplayName,
			)
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(warningColor).Render(warn))
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("y/enter to continue · n/esc to cancel"))

	case providerPhaseAPIKey:
		// Show provider title.
		sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).
			Render("Provider: " + m.activeProvider.DisplayName))
		sb.WriteString("\n\n")

		// Echo previously-collected prompt answers so the user can
		// double-check before submitting.
		for i := 0; i < m.promptIndex && i < len(m.activeProvider.Prompts); i++ {
			p := m.activeProvider.Prompts[i]
			line := lipgloss.NewStyle().Foreground(dimColor).Render("  "+p.Message+":") +
				"  " + lipgloss.NewStyle().Foreground(textColor).Render(m.metadata[p.Key])
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		if m.promptIndex > 0 {
			sb.WriteString("\n")
		}

		// Current field label.
		var fieldLabel string
		if m.promptIndex < len(m.activeProvider.Prompts) {
			fieldLabel = m.activeProvider.Prompts[m.promptIndex].Message
		} else {
			fieldLabel = "API key"
		}
		sb.WriteString(fmt.Sprintf("%s:\n", fieldLabel))
		sb.WriteString(m.apiKey.View())
		sb.WriteString("\n\n")

		// Privacy note + submit hint.
		sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).Italic(true).
			Render("the key is forwarded to the target host machine"))
		if m.errMsg != "" {
			sb.WriteString("\n")
			sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).Render(m.errMsg))
		}
		sb.WriteString("\n\n")
		hint := "enter to continue · esc to cancel"
		if m.promptIndex >= len(m.activeProvider.Prompts) {
			hint = "enter to submit · esc to cancel"
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Render(hint))

	case providerPhaseOAuthCode:
		// Two completion paths run concurrently and we poll for both:
		// locally, setup-token opens the browser and finishes via its own
		// callback (nothing to paste); remotely, the IdP shows a code to
		// paste here. The spinner signals we're already waiting, so a
		// self-completing local flow doesn't look stuck.
		sb.WriteString(m.spinner.View() + " Waiting for you to sign in…")
		sb.WriteString("\n\n")
		sb.WriteString("Open this URL if a browser didn't open automatically:\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(primaryColor).
			Render("  " + m.flow.VerificationURL))
		sb.WriteString("\n\n")
		sb.WriteString("Paste the code here if one is shown (otherwise just wait):\n\n")
		sb.WriteString("  " + m.apiKey.View())
		sb.WriteString("\n")
		if m.errMsg != "" {
			sb.WriteString("\n")
			sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).Render(m.errMsg))
		}
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("enter to submit code · esc to cancel"))

	case providerPhaseAwaiting:
		// Device flows show the URL + user_code; api-key + oauth-code
		// flows skip straight to the spinner — there's nothing for
		// the user to do externally at this point.
		if m.flow.UserCode != "" {
			sb.WriteString("In your browser, open:\n")
			sb.WriteString(lipgloss.NewStyle().Foreground(primaryColor).Render("  " + m.flow.VerificationURL))
			sb.WriteString("\n\nEnter this code:\n")
			sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(textColor).
				Render("  " + m.flow.UserCode))
			sb.WriteString("\n\n")
		}
		label := awaitingLabel(m.flowState, m.activeProvider.AuthType, isAnthropicProviderID(m.activeProvider.ProviderID))
		sb.WriteString(m.spinner.View() + " " + label)
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("esc to cancel"))

	case providerPhaseSuccess:
		sb.WriteString(lipgloss.NewStyle().Foreground(successColor).
			Render("✓ Connected " + m.activeProvider.DisplayName))
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("press any key to dismiss"))

	case providerPhaseError:
		sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).
			Render("Error: " + m.errMsg))
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("press any key to dismiss"))
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())
}

// awaitingLabel chooses the spinner label based on flow state, the
// provider's AuthType, and whether the provider is Anthropic (no
// opencode-server restart involved; credential is just persisted for
// the next claude spawn).
func awaitingLabel(state agent.DeviceFlowState, authType string, isAnthropic bool) string {
	if state == agent.DeviceFlowAuthorized {
		if isAnthropic {
			return "Saved — finalizing…"
		}
		return "Authorized — restarting OpenCode server (this can take 10–15s)…"
	}
	switch authType {
	case agent.AuthTypeDevice:
		return "Waiting for authorization…"
	case agent.AuthTypeOAuthCode:
		return "Exchanging code for token…"
	default:
		return "Saving credential…"
	}
}

// isAnthropicProviderID reports whether providerID is one of the
// Anthropic catalog entries (whose credentials live in clank's
// anthropic sink, not opencode's auth.json). Mirrors the host-side
// isAnthropicProvider, kept inline here to avoid leaking that helper
// across package boundaries.
func isAnthropicProviderID(providerID string) bool {
	return providerID == host.ProviderAnthropicClaudeCode ||
		providerID == host.ProviderAnthropicAPI
}

// providerSectionBreakpoints returns the cursor positions at the top
// of each backend section in providers — fed into the shared
// nextBreakpoint / prevBreakpoint helpers for shift+up/shift+down
// navigation. Returns nil for an empty list. Index 0 is always the
// first breakpoint; later indices are where the Backend field changes
// from the entry above.
func providerSectionBreakpoints(providers []agent.ProviderAuthInfo) []int {
	if len(providers) == 0 {
		return nil
	}
	bp := []int{0}
	for i := 1; i < len(providers); i++ {
		if providers[i].Backend != providers[i-1].Backend {
			bp = append(bp, i)
		}
	}
	return bp
}

// renderBackendSectionHeader returns a styled, full-width "── Label ──"
// rule for the given backend, used to group providers in the list view
// when no backend filter is active.
func renderBackendSectionHeader(bt agent.BackendType, width int) string {
	label := backendDisplayName(bt)
	style := lipgloss.NewStyle().Foreground(mutedColor).Bold(true)
	const prefix = "── "
	const suffix = " "
	visible := lipgloss.Width(prefix) + lipgloss.Width(label) + lipgloss.Width(suffix)
	fill := max(width-visible, 0)
	return style.Render(prefix + label + suffix + strings.Repeat("─", fill))
}

// backendDisplayName maps a BackendType to its human-readable label
// used in section headers. Unknown values fall back to the raw string
// so a future backend still renders something sensible.
func backendDisplayName(bt agent.BackendType) string {
	switch bt {
	case agent.BackendClaudeCode:
		return "Claude Code"
	case agent.BackendOpenCode:
		return "OpenCode"
	case agent.BackendCodex:
		return "Codex"
	default:
		return string(bt)
	}
}
