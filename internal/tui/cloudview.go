package tui

// cloudview.go — TUI panel for the cloud. Provider-agnostic by design:
// clank discovers the IdP from <GatewayURL>/auth-config, then runs
// standard OAuth 2.0 authorization code + PKCE against the returned
// endpoints. The gateway never sees passwords; clank never knows or
// cares which IdP (Supabase OAuth Server, Auth0, Keycloak, …) is on
// the other end.
//
// One panel, narrow phase machine, single async chain (Login). Mirrors
// the spirit of providerauth.go (one model, one bubbletea.Update, one
// View) but with simpler state since PKCE has no polling.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
)

// cloudViewPhase walks the user through OAuth login + signed-in state.
type cloudViewPhase int

const (
	cloudPhaseLoading       cloudViewPhase = iota
	cloudPhaseNotConfigured                // no gateway URL in prefs
	cloudPhaseSignedOut                    // press Enter to start
	cloudPhaseLoggingIn                    // browser opened, awaiting callback + token exchange
	cloudPhaseSignedIn                     // user-info card
	cloudPhaseError                        // message + retry
)

// --- async messages -------------------------------------------------

// cloudLoginResultMsg is delivered when the OAuth Login dance finishes
// (success or failure). err is the OAuth error sentinel when relevant.
type cloudLoginResultMsg struct {
	session *cloud.Session
	err     error
}

// cloudOpenURLPickerMsg is emitted when the user requests to change
// the cloud URL. The inbox intercepts it and opens the URL picker modal.
type cloudOpenURLPickerMsg struct{}

// cloudOpenProviderAuthMsg is emitted when the user requests to connect
// an AI provider on the cloud sandbox (signed-in only). The inbox
// intercepts it, builds a cloud.AuthCaller from the active remote's
// gateway URL + access token, and opens the provider auth modal.
type cloudOpenProviderAuthMsg struct{}

// cloudReachabilityMsg carries the result of an authenticated probe
// against the gateway. Fed into the model by Init (cold start) so the
// sidebar can leave the "checking" state without waiting for the user
// to navigate to a feature that hits the gateway.
type cloudReachabilityMsg struct{ err error }

// --- model ----------------------------------------------------------

type cloudView struct {
	width, height int
	focused       bool

	phase cloudViewPhase

	spinner spinner.Model

	// session is the most recent successful auth result.
	session *cloud.Session

	// err is rendered in cloudPhaseError, and as a banner in
	// cloudPhaseSignedIn when we want to surface a non-fatal note.
	err string

	// gatewayURL is read from the active remote's GatewayURL. Empty
	// when no remote is configured; phase reflects that.
	gatewayURL string

	// client is the gateway HTTP client (used for /auth-config
	// discovery). Nil when gatewayURL is empty.
	client *cloud.Client

	// loginCancel is the context-cancel for an in-flight login;
	// invoked on `c` or `esc` from cloudPhaseLoggingIn.
	loginCancel context.CancelFunc

	// remoteNames is the sorted list of configured remotes — rendered
	// as a "remote: dev managed enterprise (tab to switch)" line at
	// the top so the user sees what they can tab to.
	remoteNames []string
	// activeRemoteName is the currently-active entry in remoteNames.
	// Re-read on every Init so prefs edits made out of band (e.g. via
	// `clank remote switch`) take effect on the next panel open.
	activeRemoteName string

	// Reachability tracking — fed into Status() so the sidebar
	// indicator can distinguish "identity ok, server unreachable"
	// from "identity ok, all good". Updated by message handlers.
	hasCalled   bool
	lastCallErr error
}

// Status combines the disk-derived identity baseline with in-memory
// reachability tracking. Sidebar reads this every cloud Update tick.
//
// The disk baseline is authoritative for NotConfigured and Offline
// (no token → reachability is moot). Once a token is on disk we move
// through Online → Unavailable based on the most recent
// server call we've made.
func (m *cloudView) Status() cloudAuthStatus {
	base := loadCloudAuthStatus()
	if base == cloudStatusNotConfigured || base == cloudStatusOffline {
		return base
	}
	if !m.hasCalled {
		return base
	}
	if m.lastCallErr == nil {
		return cloudStatusOnline
	}
	if errors.Is(m.lastCallErr, cloud.ErrUnauthorized) {
		return cloudStatusOffline
	}
	return cloudStatusUnavailable
}

func newCloudView() cloudView {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(primaryColor)

	return cloudView{
		phase:   cloudPhaseLoading,
		spinner: sp,
	}
}

// Init loads prefs, decides the entry phase, and is otherwise quiet
// (no async work). PKCE login fires only when the user explicitly
// presses Enter.
func (m *cloudView) Init() tea.Cmd {
	prefs, _ := config.LoadPreferences()
	m.remoteNames = remoteNamesSorted(prefs)
	m.activeRemoteName = ""
	if prefs.Remote != nil {
		m.activeRemoteName = prefs.Remote.Active
	}
	p := prefs.ActiveRemote()
	m.gatewayURL = ""
	if p != nil {
		m.gatewayURL = p.GatewayURL
	}
	// Clear any session carried over from a previous remote.
	m.session = nil
	m.hasCalled = false
	m.lastCallErr = nil

	if m.gatewayURL == "" {
		m.phase = cloudPhaseNotConfigured
		m.client = nil
		return nil
	}
	m.client = cloud.New(m.gatewayURL, nil)

	if p != nil && p.AccessToken != "" && !cloudTokenExpired(p) {
		m.session = &cloud.Session{
			AccessToken:  p.AccessToken,
			RefreshToken: p.RefreshToken,
			UserID:       p.UserID,
			UserEmail:    p.UserEmail,
			ExpiresAt:    p.ExpiresAt,
		}
		m.phase = cloudPhaseSignedIn
		// Fire a probe so the sidebar can leave "checking" state.
		// Authenticated GET /ping returns 200 with a valid bearer,
		// 401 if the token's been revoked or doesn't match the
		// gateway's auth config, network error if unreachable.
		return cloudReachabilityProbe(m.gatewayURL, p.AccessToken)
	}

	m.phase = cloudPhaseSignedOut
	return nil
}

func (m *cloudView) SetSize(w, h int)  { m.width = w; m.height = h }
func (m *cloudView) SetFocused(f bool) { m.focused = f }

// Update handles the panel's messages.
func (m cloudView) Update(msg tea.Msg) (cloudView, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case cloudLoginResultMsg:
		m.loginCancel = nil
		if msg.err != nil {
			if errors.Is(msg.err, cloud.ErrLoginCancelled) {
				m.phase = cloudPhaseSignedOut
				m.err = ""
				return m, nil
			}
			m.phase = cloudPhaseError
			m.err = "sign-in failed: " + msg.err.Error()
			return m, nil
		}
		if err := persistSession(msg.session); err != nil {
			// In-memory session is useless if disk doesn't match —
			// next restart loses it. Tell the user instead of
			// rendering a false signed-in card.
			m.phase = cloudPhaseError
			m.err = "save session: " + err.Error()
			return m, nil
		}
		m.session = msg.session
		m.phase = cloudPhaseSignedIn
		m.err = ""
		// The token-exchange round-trip just succeeded, so we know
		// the IdP is reachable and minted us a valid token. Flip the
		// reachability flag so the sidebar leaves "checking" without
		// waiting on a follow-up gateway probe.
		m.hasCalled = true
		m.lastCallErr = nil
		return m, nil

	case cloudReachabilityMsg:
		m.hasCalled = true
		m.lastCallErr = msg.err
		if errors.Is(msg.err, cloud.ErrUnauthorized) {
			// Token was revoked or no longer matches the gateway's
			// auth config. Wipe disk so the next Init() doesn't
			// reload a dead token and render a stale signed-in card.
			_ = clearSession()
			m.session = nil
			m.phase = cloudPhaseSignedOut
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m cloudView) handleKey(msg tea.KeyPressMsg) (cloudView, tea.Cmd) {
	// Tab cycles through configured remotes regardless of phase, so a
	// signed-out / signed-in / not-configured state on one remote
	// doesn't trap the user out of switching. Persists active, then
	// re-Inits so the panel reflects the new selection.
	if len(m.remoteNames) > 1 && key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) {
		next := nextRemoteName(m.remoteNames, m.activeRemoteName)
		if next != m.activeRemoteName {
			_ = switchToRemote(next)
			cmd := m.Init()
			return m, cmd
		}
	}

	switch m.phase {
	case cloudPhaseNotConfigured:
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			return m, func() tea.Msg { return cloudOpenURLPickerMsg{} }
		}
		return m, nil

	case cloudPhaseSignedOut:
		// Enter opens the browser and starts PKCE; any other key is
		// a no-op so the user can navigate back to the sidebar.
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			m.err = ""
			m.phase = cloudPhaseLoggingIn
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			m.loginCancel = cancel
			return m, tea.Batch(m.spinner.Tick, m.loginCmd(ctx))
		}
		// 'u' jumps to URL picker even from signed-out (e.g. user
		// configured the wrong gateway and needs to re-enter it).
		if key.Matches(msg, key.NewBinding(key.WithKeys("u"))) {
			return m, func() tea.Msg { return cloudOpenURLPickerMsg{} }
		}
		return m, nil

	case cloudPhaseLoggingIn:
		// 'c' / esc cancels the in-flight login.
		if key.Matches(msg, key.NewBinding(key.WithKeys("c", "esc"))) {
			if m.loginCancel != nil {
				m.loginCancel()
				m.loginCancel = nil
			}
			m.phase = cloudPhaseSignedOut
			return m, nil
		}
		return m, nil

	case cloudPhaseSignedIn:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("o"))):
			_ = clearSession()
			m.session = nil
			m.err = ""
			m.phase = cloudPhaseSignedOut
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("u"))):
			return m, func() tea.Msg { return cloudOpenURLPickerMsg{} }
		case key.Matches(msg, key.NewBinding(key.WithKeys("p"))):
			return m, func() tea.Msg { return cloudOpenProviderAuthMsg{} }
		}
		return m, nil

	case cloudPhaseError:
		// Any key returns to the prior usable state (signed-out, so
		// they can retry the flow).
		m.phase = cloudPhaseSignedOut
		m.err = ""
		return m, nil
	}
	return m, nil
}

// --- async commands -------------------------------------------------

// cloudReachabilityProbe fires an authenticated GET /ping against the
// gateway and returns a cloudReachabilityMsg with the verdict. Used by
// Init() on cold start: when prefs already hold a token, the sidebar
// would otherwise stay in "checking" forever (no other code path flips
// hasCalled). The probe gives us a reachability answer in one round-trip.
//
// Bounded by a short timeout so a slow/unreachable gateway doesn't
// leave the probe goroutine hanging — Status() simply stays Checking
// until the timeout fires and reports the failure.
func cloudReachabilityProbe(gatewayURL, accessToken string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gatewayURL, "/")+"/ping", nil)
		if err != nil {
			return cloudReachabilityMsg{err: err}
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return cloudReachabilityMsg{err: err}
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			return cloudReachabilityMsg{err: nil}
		case http.StatusUnauthorized:
			return cloudReachabilityMsg{err: cloud.ErrUnauthorized}
		default:
			return cloudReachabilityMsg{err: fmt.Errorf("ping: status %d", resp.StatusCode)}
		}
	}
}

// loginCmd kicks off the full PKCE flow in a goroutine and returns
// the result via cloudLoginResultMsg. It performs two HTTP roundtrips:
//  1. GET <gateway>/auth-config (discover OAuth 2.0 endpoints + client_id)
//  2. The OAuth dance (browser open, callback, token exchange)
//
// Both run inside the supplied ctx; cancelling ctx aborts the flow.
func (m *cloudView) loginCmd(ctx context.Context) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		// Discover IdP via /auth-config.
		discoverCtx, cancelDiscover := context.WithTimeout(ctx, 10*time.Second)
		defer cancelDiscover()
		cfg, err := client.FetchAuthConfig(discoverCtx)
		if err != nil {
			return cloudLoginResultMsg{err: fmt.Errorf("discover IdP: %w", err)}
		}
		oauth := &cloud.OAuthClient{
			AuthorizeEndpoint: cfg.AuthorizeEndpoint,
			TokenEndpoint:     cfg.TokenEndpoint,
			ClientID:          cfg.ClientID,
			Scopes:            cfg.Scopes,
			Provider:          cfg.DefaultProvider,
			CallbackPort:      cfg.CallbackPort,
			// Discard manual-paste prompts: stderr writes from
			// within a Bubble Tea program corrupt the display.
			// TODO: surface as a cloudPhaseError card instead.
			Prompt: io.Discard,
		}
		sess, err := oauth.Login(ctx)
		return cloudLoginResultMsg{session: sess, err: err}
	}
}

// --- view -----------------------------------------------------------

func (m cloudView) View() string {
	header := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render("☁  Cloud")

	// Remote selector — rendered between header and phase body so the
	// user sees what they can tab to. Hidden when only one (or zero)
	// remote is configured; nothing to switch to.
	var selector string
	if len(m.remoteNames) > 1 {
		muted := lipgloss.NewStyle().Foreground(mutedColor)
		active := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)
		parts := []string{muted.Render("remote:")}
		for _, n := range m.remoteNames {
			if n == m.activeRemoteName {
				parts = append(parts, active.Render("["+n+"]"))
			} else {
				parts = append(parts, muted.Render(n))
			}
		}
		parts = append(parts, muted.Render("(tab to switch)"))
		selector = strings.Join(parts, " ") + "\n\n"
	}

	var body string
	switch m.phase {
	case cloudPhaseLoading:
		body = lipgloss.NewStyle().Foreground(mutedColor).Render(m.spinner.View() + " loading…")
	case cloudPhaseNotConfigured:
		body = m.viewNotConfigured()
	case cloudPhaseSignedOut:
		body = m.viewSignedOut()
	case cloudPhaseLoggingIn:
		body = m.viewLoggingIn()
	case cloudPhaseSignedIn:
		body = m.viewSignedIn()
	case cloudPhaseError:
		body = lipgloss.NewStyle().Foreground(dangerColor).Render(m.err) + "\n\n" +
			lipgloss.NewStyle().Foreground(mutedColor).Render("press any key to try again")
	}

	return header + "\n\n" + selector + body
}

func (m cloudView) viewNotConfigured() string {
	muted := lipgloss.NewStyle().Foreground(mutedColor)
	return muted.Render("Cloud not configured.\n\npress Enter to set the gateway URL.")
}

func (m cloudView) viewSignedOut() string {
	muted := lipgloss.NewStyle().Foreground(mutedColor)
	text := lipgloss.NewStyle().Foreground(textColor)
	parts := []string{
		text.Render("Connect this device to the cloud."),
		"",
		muted.Render("press Enter to sign in via your browser."),
		"",
		muted.Render("u: change cloud URL"),
	}
	if m.err != "" {
		parts = append([]string{lipgloss.NewStyle().Foreground(dangerColor).Render(m.err), ""}, parts...)
	}
	return strings.Join(parts, "\n")
}

func (m cloudView) viewLoggingIn() string {
	muted := lipgloss.NewStyle().Foreground(mutedColor)
	text := lipgloss.NewStyle().Foreground(textColor)
	rows := []string{
		text.Render(m.spinner.View() + " opening browser…"),
		"",
		muted.Render("complete sign-in in the browser tab that opened."),
		"",
		muted.Render("c / esc: cancel"),
	}
	return strings.Join(rows, "\n")
}

func (m cloudView) viewSignedIn() string {
	muted := lipgloss.NewStyle().Foreground(mutedColor)
	text := lipgloss.NewStyle().Foreground(textColor)

	rows := []string{}

	if m.session != nil {
		rows = append(rows,
			text.Render("Signed in as ")+text.Bold(true).Render(m.session.UserEmail),
			muted.Render("user_id   "+truncate(m.session.UserID, 36)),
		)
		if m.session.ExpiresAt > 0 {
			rows = append(rows, muted.Render("expires   "+humanTTL(time.Until(time.Unix(m.session.ExpiresAt, 0)))))
		}
		rows = append(rows, "")
	}

	if m.gatewayURL != "" {
		rows = append(rows, muted.Render("gateway   "+m.gatewayURL), "")
	}

	if m.err != "" {
		rows = append(rows, muted.Render(m.err), "")
	}

	rows = append(rows, muted.Render("p: connect provider (in sandbox)   u: change URL   o: sign out"))
	return strings.Join(rows, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func humanTTL(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// --- pref helpers ---------------------------------------------------

func cloudTokenExpired(p *config.Remote) bool {
	if p == nil || p.ExpiresAt == 0 {
		return false
	}
	return time.Now().Unix() > p.ExpiresAt-30
}

func persistSession(s *cloud.Session) error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		profile := ensureActiveProfile(p)
		profile.AccessToken = s.AccessToken
		profile.RefreshToken = s.RefreshToken
		profile.UserEmail = s.UserEmail
		profile.UserID = s.UserID
		profile.ExpiresAt = s.ExpiresAt
	})
}

func clearSession() error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		profile := p.ActiveRemote()
		if profile == nil {
			return
		}
		profile.AccessToken = ""
		profile.RefreshToken = ""
		profile.UserEmail = ""
		profile.UserID = ""
		profile.ExpiresAt = 0
	})
}

// remoteNamesSorted returns the configured remote names in
// deterministic alphabetical order. Empty when no remotes exist.
func remoteNamesSorted(prefs config.Preferences) []string {
	if prefs.Remote == nil || len(prefs.Remote.Profiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(prefs.Remote.Profiles))
	for k := range prefs.Remote.Profiles {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// switchToRemote sets the named remote as active in preferences. Used
// by the cloud panel's tab handler.
func switchToRemote(name string) error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		if p.Remote == nil || p.Remote.Profiles == nil {
			return
		}
		if _, ok := p.Remote.Profiles[name]; !ok {
			return
		}
		p.Remote.Active = name
	})
}

// nextRemoteName returns the next remote in the sorted list after
// current, wrapping around. Empty string out when names is empty.
func nextRemoteName(names []string, current string) string {
	if len(names) == 0 {
		return ""
	}
	for i, n := range names {
		if n == current {
			return names[(i+1)%len(names)]
		}
	}
	return names[0]
}

// ensureActiveProfile resolves (or creates) the active cloud profile
// so the OAuth grant has somewhere to write. Used by persistSession;
// clear is a no-op when nothing is active.
func ensureActiveProfile(p *config.Preferences) *config.Remote {
	if p.Remote == nil {
		p.Remote = &config.RemoteConfig{Active: "default", Profiles: map[string]*config.Remote{}}
	}
	if p.Remote.Profiles == nil {
		p.Remote.Profiles = map[string]*config.Remote{}
	}
	if p.Remote.Active == "" {
		p.Remote.Active = "default"
	}
	if _, ok := p.Remote.Profiles[p.Remote.Active]; !ok {
		p.Remote.Profiles[p.Remote.Active] = &config.Remote{}
	}
	return p.Remote.Profiles[p.Remote.Active]
}
