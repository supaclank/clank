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
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

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
	// providerPhaseOAuthCode is the "paste authorization code" step
	// for oauth-code providers (Anthropic Claude subscription). The
	// view shows the authorize URL printed by the host's PTY-relayed
	// `claude setup-token` + a textinput for the code the user
	// copied from the IdP's hosted callback page. On submit, we
	// transition straight to awaiting while the host writes the
	// code into the CLI's stdin and captures the token.
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
	hub      *daemonclient.Client
	hostname string

	// backend, when non-empty, scopes the provider list to those
	// consumed by that agent CLI (opencode | claude-code). The model
	// picker passes its current backend through so a claude-code
	// session's "+ Connect provider…" shows only Anthropic entries
	// and an opencode session shows only the opencode-managed ones.
	// Empty = no filter (settings-page entry into the modal).
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
}

func newProviderAuthModel(c *daemonclient.Client, hostname string, backend agent.BackendType) providerAuthModel {
	if hostname == "" {
		hostname = host.HostLocal
	}
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
		hub:      c,
		hostname: hostname,
		backend:  backend,
		phase:    providerPhaseLoading,
		spinner:  sp,
		apiKey:   ti,
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
		// oauth-code flows pause here for the user to paste the code
		// shown on the IdP's redirect page; device + api-key flows go
		// straight to awaiting + polling.
		if m.activeProvider.AuthType == agent.AuthTypeOAuthCode {
			m.phase = providerPhaseOAuthCode
			m.configureInputForOAuthCode()
			return m, m.apiKey.Focus()
		}
		m.phase = providerPhaseAwaiting
		return m, m.statusCmd()

	case providerPollTickMsg:
		if m.phase != providerPhaseAwaiting {
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
	hub := m.hub
	hostname := m.hostname
	backend := m.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		providers, err := hub.Host(hostname).ListAuthProviders(ctx, backend)
		return providerListLoadedMsg{providers: providers, err: err}
	}
}

func (m providerAuthModel) startFlowCmd(providerID string) tea.Cmd {
	hub := m.hub
	hostname := m.hostname
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start, err := hub.Host(hostname).StartAuthDeviceFlow(ctx, providerID)
		return providerStartedMsg{start: start, err: err}
	}
}

func (m providerAuthModel) submitAPIKeyCmd(providerID, key string, metadata map[string]string) tea.Cmd {
	hub := m.hub
	hostname := m.hostname
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start, err := hub.Host(hostname).SubmitAuthAPIKey(ctx, providerID, key, metadata)
		return providerStartedMsg{start: start, err: err}
	}
}

// startOAuthCodeFlowCmd asks the host to spawn `claude setup-token`
// and return the authorize URL it prints. Reuses providerStartedMsg
// — the start payload carries FlowID + VerificationURL (UserCode is
// empty, since this flow has no shown user_code).
func (m providerAuthModel) startOAuthCodeFlowCmd(providerID string) tea.Cmd {
	hub := m.hub
	hostname := m.hostname
	return func() tea.Msg {
		// Generous timeout — the host's PTY-spawn + URL extraction
		// can take a few seconds on a cold sprite while the CLI's
		// startup banner animates.
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		start, err := hub.Host(hostname).StartAuthOAuthCodeFlow(ctx, providerID)
		return providerStartedMsg{start: start, err: err}
	}
}

// submitAuthCodeCmd delivers the user-pasted code to the host. The
// host writes it into setup-token's stdin and waits for the token,
// so this call can take a few seconds. On return, we synthesize a
// providerStatusMsg so the existing terminal-state handler in Update
// flips the phase to success/error without needing a new message
// type.
func (m providerAuthModel) submitAuthCodeCmd(providerID, flowID, code string) tea.Cmd {
	hub := m.hub
	hostname := m.hostname
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := hub.Host(hostname).SubmitAuthCode(ctx, providerID, flowID, code); err != nil {
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
	hub := m.hub
	hostname := m.hostname
	provider := m.activeProvider.ProviderID
	flowID := m.flow.FlowID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		status, err := hub.Host(hostname).AuthFlowStatus(ctx, provider, flowID)
		return providerStatusMsg{status: status, err: err}
	}
}

func (m providerAuthModel) pollTickCmd() tea.Cmd {
	return tea.Tick(providerAuthPollInterval, func(time.Time) tea.Msg {
		return providerPollTickMsg{}
	})
}

func (m providerAuthModel) cancelFlowCmd() tea.Cmd {
	hub := m.hub
	hostname := m.hostname
	provider := m.activeProvider.ProviderID
	flowID := m.flow.FlowID
	return func() tea.Msg {
		if flowID == "" {
			return providerAuthCancelMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hub.Host(hostname).CancelAuthFlow(ctx, provider, flowID)
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

	case providerPhaseList:
		if len(m.providers) == 0 {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Render("  no providers available"))
		} else {
			for i, p := range m.providers {
				prefix := "  "
				labelStyle := lipgloss.NewStyle().Foreground(textColor)
				if i == m.cursor {
					prefix = "▶ "
					labelStyle = labelStyle.Bold(true).Background(primaryColor)
				}
				status := lipgloss.NewStyle().Foreground(dimColor).Render("not connected")
				if p.Connected {
					status = lipgloss.NewStyle().Foreground(successColor).Render("connected")
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
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("↑↓ navigate · enter select · esc cancel"))

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
		// User opens VerificationURL in a browser, logs in, and the
		// IdP's hosted callback page shows them a code to copy back
		// here.
		sb.WriteString("Open this URL in your browser:\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(primaryColor).
			Render("  " + m.flow.VerificationURL))
		sb.WriteString("\n\n")
		sb.WriteString("After signing in, paste the code shown on the redirect page:\n\n")
		sb.WriteString("  " + m.apiKey.View())
		sb.WriteString("\n")
		if m.errMsg != "" {
			sb.WriteString("\n")
			sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).Render(m.errMsg))
		}
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("enter to submit · esc to cancel"))

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
