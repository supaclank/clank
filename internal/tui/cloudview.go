package tui

// cloudview.go — TUI panel for the cloud. Provider-agnostic
// by design: clank speaks RFC 8628 device flow to the cloud's URL and
// nothing else. The cloud owns the user-auth mechanism
// (Supabase, OAuth, magic link, …) and exposes it via its /connect
// web page; clank never sees the auth provider.
//
// Mirrors providerauth.go's device-flow pattern: a single tea.Model
// with a phase enum and async HTTP wrapped in tea.Cmd.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
)

// cloudViewPhase walks the user through the device-flow grant.
type cloudViewPhase int

const (
	cloudPhaseLoading       cloudViewPhase = iota
	cloudPhaseNotConfigured                // no cloud_url in prefs
	cloudPhaseSignedOut                    // press Enter to start
	cloudPhaseStarting                     // device/start in flight
	cloudPhaseAwaiting                     // showing user_code; polling
	cloudPhaseFetchingMe                   // success → fetching /me
	cloudPhaseSignedIn                     // user-info card
	cloudPhaseError                        // message + retry
)

// --- async messages -------------------------------------------------

type cloudDeviceStartedMsg struct {
	resp *cloud.DeviceStartResponse
	err  error
}

type cloudDevicePollMsg struct{}

type cloudDevicePollResultMsg struct {
	session *cloud.Session
	err     error
}

type cloudMeResultMsg struct {
	me  *cloud.MeResponse
	err error
}

// cloudOpenURLPickerMsg is emitted when the user requests to change
// the cloud URL. The inbox intercepts it and opens the URL picker modal.
type cloudOpenURLPickerMsg struct{}

// --- model ----------------------------------------------------------

type cloudView struct {
	width, height int
	focused       bool

	phase cloudViewPhase

	// device-flow state, populated after StartDeviceFlow.
	device *cloud.DeviceStartResponse

	// pollInterval is the per-flow cadence the cloud asks us to use.
	// May be increased on ErrSlowDown per RFC 8628 §3.5.
	pollInterval time.Duration

	// pollDeadline marks when device.ExpiresIn elapses; we stop
	// polling and surface ErrExpiredToken locally if the cloud doesn't.
	pollDeadline time.Time

	spinner spinner.Model

	// session is populated on a successful poll; persisted to prefs
	// before transitioning to cloudPhaseFetchingMe.
	session *cloud.Session

	// me is the most recent /me response; rendered in cloudPhaseSignedIn.
	me *cloud.MeResponse

	// err carries the message rendered in cloudPhaseError (and as a
	// banner in cloudPhaseSignedIn when /me reported ErrNotSignedUp).
	err string

	cloudURL string
	client   *cloud.Client

	// Reachability tracking — fed into Status() so the sidebar
	// indicator can distinguish "identity ok, server unreachable"
	// from "identity ok, all good". Updated by message handlers
	// for /me and the device-flow calls.
	hasCalled   bool
	lastCallErr error
}

// Status combines the disk-derived identity baseline with in-memory
// reachability tracking. Sidebar reads this every cloud Update tick.
//
// The disk baseline is authoritative for NotConfigured and Offline
// (no token → reachability is moot). Once a token is on disk we move
// through Checking → Online | Unavailable based on the most recent
// server call we've made.
func (m *cloudView) Status() cloudAuthStatus {
	base := loadCloudAuthStatus()
	if base != cloudStatusChecking {
		return base
	}
	if !m.hasCalled {
		return cloudStatusChecking
	}
	if m.lastCallErr == nil {
		return cloudStatusOnline
	}
	if errors.Is(m.lastCallErr, cloud.ErrUnauthorized) {
		// Defensive: clearSession() should already have flipped the
		// disk baseline to Offline before we get here.
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

// Init loads prefs, decides the entry phase, and (if there's a saved
// session) kicks off a /me to verify it.
func (m *cloudView) Init() tea.Cmd {
	prefs, _ := config.LoadPreferences()
	m.cloudURL = ""
	if prefs.Cloud != nil {
		m.cloudURL = prefs.Cloud.CloudURL
	}
	if m.cloudURL == "" {
		m.phase = cloudPhaseNotConfigured
		return nil
	}
	m.client = cloud.New(m.cloudURL, nil)

	if prefs.Cloud != nil && prefs.Cloud.AccessToken != "" && !cloudTokenExpired(prefs.Cloud) {
		m.session = &cloud.Session{
			AccessToken:  prefs.Cloud.AccessToken,
			RefreshToken: prefs.Cloud.RefreshToken,
			UserID:       prefs.Cloud.UserID,
			UserEmail:    prefs.Cloud.UserEmail,
			ExpiresAt:    prefs.Cloud.ExpiresAt,
		}
		m.phase = cloudPhaseFetchingMe
		return tea.Batch(m.spinner.Tick, m.fetchMeCmd())
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

	case cloudDeviceStartedMsg:
		if msg.err != nil {
			m.phase = cloudPhaseError
			m.err = "starting device flow: " + msg.err.Error()
			return m, nil
		}
		m.device = msg.resp
		m.pollInterval = time.Duration(maxInt(msg.resp.Interval, 1)) * time.Second
		m.pollDeadline = time.Now().Add(time.Duration(msg.resp.ExpiresIn) * time.Second)
		m.phase = cloudPhaseAwaiting
		// Nudge the user's browser if we can — non-fatal if it fails.
		go openBrowser(msg.resp.VerificationURIComplete, msg.resp.VerificationURI)
		// Start the poll loop.
		return m, tea.Batch(m.spinner.Tick, m.scheduleNextPoll())

	case cloudDevicePollMsg:
		// Ignore stray ticks once we've moved past the awaiting phase.
		if m.phase != cloudPhaseAwaiting {
			return m, nil
		}
		if time.Now().After(m.pollDeadline) {
			m.phase = cloudPhaseError
			m.err = "device code expired — press any key to start over"
			return m, nil
		}
		return m, m.pollOnceCmd()

	case cloudDevicePollResultMsg:
		switch {
		case msg.err == nil:
			// Approved — persist + fetch /me.
			m.session = msg.session
			_ = persistSession(msg.session)
			m.phase = cloudPhaseFetchingMe
			return m, tea.Batch(m.spinner.Tick, m.fetchMeCmd())
		case errors.Is(msg.err, cloud.ErrAuthorizationPending):
			return m, m.scheduleNextPoll()
		case errors.Is(msg.err, cloud.ErrSlowDown):
			m.pollInterval += 5 * time.Second
			return m, m.scheduleNextPoll()
		case errors.Is(msg.err, cloud.ErrAccessDenied):
			m.phase = cloudPhaseError
			m.err = "the request was denied"
			return m, nil
		case errors.Is(msg.err, cloud.ErrExpiredToken):
			m.phase = cloudPhaseError
			m.err = "device code expired — press any key to start over"
			return m, nil
		default:
			m.phase = cloudPhaseError
			m.err = "polling: " + msg.err.Error()
			return m, nil
		}

	case cloudMeResultMsg:
		m.hasCalled = true
		m.lastCallErr = msg.err
		if errors.Is(msg.err, cloud.ErrUnauthorized) {
			m.session = nil
			_ = clearSession()
			m.phase = cloudPhaseSignedOut
			m.err = "session expired; sign in again"
			return m, nil
		}
		if errors.Is(msg.err, cloud.ErrNotSignedUp) {
			m.me = nil
			m.phase = cloudPhaseSignedIn
			m.err = "Authenticated, but you haven't claimed a handle yet."
			return m, nil
		}
		if msg.err != nil {
			m.phase = cloudPhaseError
			m.err = "fetching profile: " + msg.err.Error()
			return m, nil
		}
		m.me = msg.me
		m.err = ""
		m.phase = cloudPhaseSignedIn
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m cloudView) handleKey(msg tea.KeyPressMsg) (cloudView, tea.Cmd) {
	switch m.phase {
	case cloudPhaseNotConfigured:
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			return m, func() tea.Msg { return cloudOpenURLPickerMsg{} }
		}
		return m, nil

	case cloudPhaseSignedOut:
		// Enter starts the device flow; any other key is a no-op so
		// the user can navigate back to the sidebar.
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			m.phase = cloudPhaseStarting
			m.err = ""
			return m, tea.Batch(m.spinner.Tick, m.startDeviceCmd())
		}
		return m, nil

	case cloudPhaseAwaiting:
		// 'c' cancels and returns to signed-out.
		if key.Matches(msg, key.NewBinding(key.WithKeys("c", "esc"))) {
			m.device = nil
			m.phase = cloudPhaseSignedOut
			return m, nil
		}
		return m, nil

	case cloudPhaseSignedIn:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("o"))):
			if m.session != nil && m.client != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				m.client.SignOut(ctx, m.session.AccessToken)
			}
			_ = clearSession()
			m.session = nil
			m.me = nil
			m.err = ""
			m.phase = cloudPhaseSignedOut
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("r"))):
			if m.session != nil {
				m.phase = cloudPhaseFetchingMe
				return m, tea.Batch(m.spinner.Tick, m.fetchMeCmd())
			}
		}
		return m, nil

	case cloudPhaseError:
		// Any key returns to the prior usable state.
		m.phase = cloudPhaseSignedOut
		m.err = ""
		m.device = nil
		return m, nil
	}
	return m, nil
}

// --- async commands -------------------------------------------------

func (m *cloudView) startDeviceCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := client.StartDeviceFlow(ctx)
		return cloudDeviceStartedMsg{resp: resp, err: err}
	}
}

// scheduleNextPoll waits one pollInterval, then fires a
// cloudDevicePollMsg which Update interprets as "go ahead and poll".
// Splitting the wait from the request makes the wait cancelable
// (ESC during cloudPhaseAwaiting transitions to signed-out and any
// in-flight tick becomes a no-op).
func (m *cloudView) scheduleNextPoll() tea.Cmd {
	d := m.pollInterval
	return tea.Tick(d, func(time.Time) tea.Msg {
		return cloudDevicePollMsg{}
	})
}

func (m *cloudView) pollOnceCmd() tea.Cmd {
	client := m.client
	deviceCode := ""
	if m.device != nil {
		deviceCode = m.device.DeviceCode
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s, err := client.PollDeviceFlow(ctx, deviceCode)
		return cloudDevicePollResultMsg{session: s, err: err}
	}
}

func (m *cloudView) fetchMeCmd() tea.Cmd {
	client := m.client
	tok := ""
	if m.session != nil {
		tok = m.session.AccessToken
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		me, err := client.Me(ctx, tok)
		return cloudMeResultMsg{me: me, err: err}
	}
}

// openBrowser tries to launch the user's browser. uriComplete includes
// the user_code as a query param so the user doesn't have to type it.
// Falls back to uri if uriComplete is empty.
func openBrowser(uriComplete, uri string) {
	target := uriComplete
	if target == "" {
		target = uri
	}
	if target == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Run()
}

// --- view -----------------------------------------------------------

func (m cloudView) View() string {
	header := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render("☁  Cloud")

	var body string
	switch m.phase {
	case cloudPhaseLoading:
		body = lipgloss.NewStyle().Foreground(mutedColor).Render(m.spinner.View() + " loading…")
	case cloudPhaseNotConfigured:
		body = m.viewNotConfigured()
	case cloudPhaseSignedOut:
		body = m.viewSignedOut()
	case cloudPhaseStarting:
		body = lipgloss.NewStyle().Foreground(mutedColor).Render(m.spinner.View() + " starting device flow…")
	case cloudPhaseAwaiting:
		body = m.viewAwaiting()
	case cloudPhaseFetchingMe:
		body = lipgloss.NewStyle().Foreground(mutedColor).Render(m.spinner.View() + " fetching profile…")
	case cloudPhaseSignedIn:
		body = m.viewSignedIn()
	case cloudPhaseError:
		body = lipgloss.NewStyle().Foreground(dangerColor).Render(m.err) + "\n\n" +
			lipgloss.NewStyle().Foreground(mutedColor).Render("press any key to try again")
	}

	return header + "\n\n" + body
}

func (m cloudView) viewNotConfigured() string {
	muted := lipgloss.NewStyle().Foreground(mutedColor)
	return muted.Render("Cloud credentials not set up.\n\npress Enter to configure.")
}

func (m cloudView) viewSignedOut() string {
	muted := lipgloss.NewStyle().Foreground(mutedColor)
	text := lipgloss.NewStyle().Foreground(textColor)
	parts := []string{
		text.Render("Connect this device to the cloud."),
		"",
		muted.Render("press Enter to sign in."),
	}
	if m.err != "" {
		parts = append([]string{lipgloss.NewStyle().Foreground(dangerColor).Render(m.err), ""}, parts...)
	}
	return strings.Join(parts, "\n")
}

func (m cloudView) viewAwaiting() string {
	muted := lipgloss.NewStyle().Foreground(mutedColor)
	text := lipgloss.NewStyle().Foreground(textColor).Bold(true)
	primary := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)

	uri := ""
	code := ""
	if m.device != nil {
		uri = m.device.VerificationURI
		code = m.device.UserCode
	}

	rows := []string{
		text.Render("Visit:"),
		"  " + primary.Render(uri),
		"",
		text.Render("Enter code:"),
		"  " + primary.Render(code),
		"",
		muted.Render(m.spinner.View() + " waiting for confirmation…"),
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

	if m.me != nil {
		if len(m.me.Hubs) > 0 {
			h := m.me.Hubs[0]
			rows = append(rows,
				text.Bold(true).Render("Hub"),
				muted.Render("subdomain "+h.Subdomain),
				muted.Render("region    "+h.Region),
				muted.Render("status    "+h.Status),
				muted.Render("url       "+h.PublicURL),
				"",
			)
		}
		if len(m.me.Hosts) > 0 {
			h := m.me.Hosts[0]
			rows = append(rows,
				text.Bold(true).Render("Host"),
				muted.Render("provider  "+h.Provider),
				muted.Render("hostname  "+h.Hostname),
				muted.Render("status    "+h.Status),
				"",
			)
		} else if len(m.me.Hubs) > 0 {
			rows = append(rows, muted.Render("(no host yet — POST /provision against the cloud to create one)"), "")
		}
	} else if m.err != "" {
		rows = append(rows, muted.Render(m.err), "")
	}

	rows = append(rows, muted.Render("r: refresh   o: sign out"))
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- pref helpers ---------------------------------------------------

func cloudTokenExpired(c *config.CloudPreference) bool {
	if c == nil || c.ExpiresAt == 0 {
		return false
	}
	return time.Now().Unix() > c.ExpiresAt-30
}

func persistSession(s *cloud.Session) error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		if p.Cloud == nil {
			p.Cloud = &config.CloudPreference{}
		}
		p.Cloud.AccessToken = s.AccessToken
		p.Cloud.RefreshToken = s.RefreshToken
		p.Cloud.UserEmail = s.UserEmail
		p.Cloud.UserID = s.UserID
		p.Cloud.ExpiresAt = s.ExpiresAt
	})
}

func clearSession() error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		if p.Cloud == nil {
			return
		}
		p.Cloud.AccessToken = ""
		p.Cloud.RefreshToken = ""
		p.Cloud.UserEmail = ""
		p.Cloud.UserID = ""
		p.Cloud.ExpiresAt = 0
	})
}
