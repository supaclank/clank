package tui

// Voice integration for the TUI. Provides push-to-talk (hold SPACE to
// record, release to stop) using the daemon's voice API and local audio
// devices. Voice state lives on InboxModel so it persists across
// inbox <-> session navigation.
//
// Push-to-talk relies on Bubble Tea v2's KeyReleaseMsg which requires
// the Kitty keyboard protocol. Terminals that do not support this
// protocol (e.g. macOS Terminal.app) will not deliver KeyReleaseMsg.
// At startup we detect support via KeyboardEnhancementsMsg; if the
// terminal lacks it, pressing SPACE shows an informational popup
// instead of starting voice.
//
// Turn signals (start/stop speaking) are sent as in-band WebSocket text
// messages on the same connection that carries audio, guaranteeing
// message ordering and eliminating the race between HTTP POSTs and
// audio data that plagued the old architecture.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
	"github.com/acksell/mindmouth/audio"
	"github.com/coder/websocket"
)

// voiceState holds the runtime state for a TUI voice session.
type voiceState struct {
	active    bool // voice session is connected to the daemon
	starting  bool // voiceStartAndListen command is in flight
	recording bool // mic is unmuted, user is speaking
	speaking  bool // assistant is currently outputting audio

	recorder    *audio.Recorder
	player      *audio.Player
	wsConn      *websocket.Conn
	cancelAudio context.CancelFunc // cancels the audio goroutines
}

// --- Bubble Tea messages ---

// voiceStartResultMsg is sent after the voice session startup attempt.
type voiceStartResultMsg struct{ err error }

// voiceStopResultMsg is sent after the voice session teardown.
type voiceStopResultMsg struct{ err error }

// voiceListenResultMsg is sent after unmuting the mic.
type voiceListenResultMsg struct{ err error }

// voiceUnlistenResultMsg is sent after muting the mic.
type voiceUnlistenResultMsg struct{ err error }

// voiceAudioErrMsg is sent when an audio goroutine encounters an error.
type voiceAudioErrMsg struct{ err error }

// --- Commands (methods on InboxModel) ---

// startVoice initialises local audio devices and opens the WebSocket to the
// daemon. The voice session is created server-side when the WebSocket
// connects. The mic starts muted. Audio send/receive goroutines run in
// the background until cancelAudio is called.
func (m *InboxModel) startVoice() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		recorder, err := audio.NewRecorder()
		if err != nil {
			return voiceStartResultMsg{err: fmt.Errorf("init microphone: %w", err)}
		}

		player, err := audio.NewPlayer()
		if err != nil {
			recorder.Close()
			return voiceStartResultMsg{err: fmt.Errorf("init speaker: %w", err)}
		}

		wsConn, err := m.client.VoiceAudioStream(ctx)
		if err != nil {
			recorder.Close()
			player.Close()
			return voiceStartResultMsg{err: fmt.Errorf("connect audio stream: %w", err)}
		}

		// Start mic capture — muted by default.
		recorder.Mute()
		audioCtx, cancelAudio := context.WithCancel(ctx)
		micCh, err := recorder.Record(audioCtx)
		if err != nil {
			cancelAudio()
			recorder.Close()
			player.Close()
			wsConn.CloseNow()
			return voiceStartResultMsg{err: fmt.Errorf("start recording: %w", err)}
		}

		// Goroutine: send mic PCM to daemon via WebSocket.
		go func() {
			for pcm := range micCh {
				if err := wsConn.Write(audioCtx, websocket.MessageBinary, pcm); err != nil {
					log.Printf("voice tui: ws write error: %v", err)
					return
				}
			}
		}()

		// Goroutine: receive speaker PCM from daemon, play locally.
		go func() {
			for {
				_, data, err := wsConn.Read(audioCtx)
				if err != nil {
					return
				}
				// Zero-length binary message = flush signal (barge-in).
				if len(data) == 0 {
					player.Flush()
					continue
				}
				player.Enqueue(data)
			}
		}()

		// Store state on the model. This is safe because the Bubble Tea
		// runtime processes the returned message synchronously before any
		// other Update call.
		m.voice.recorder = recorder
		m.voice.player = player
		m.voice.wsConn = wsConn
		m.voice.cancelAudio = cancelAudio

		return voiceStartResultMsg{err: nil}
	}
}

// stopVoice tears down the voice session and releases all resources.
func (m *InboxModel) stopVoice() tea.Cmd {
	return func() tea.Msg {
		m.cleanupVoice()
		return voiceStopResultMsg{}
	}
}

// voiceListen unmutes the mic and sends an in-band turn_start signal
// to the daemon over the existing WebSocket.
func (m *InboxModel) voiceListen() tea.Cmd {
	return func() tea.Msg {
		if m.voice.recorder != nil {
			m.voice.recorder.Unmute()
		}
		if err := sendTurnSignal(m.voice.wsConn, "turn_start"); err != nil {
			return voiceListenResultMsg{err: err}
		}
		return voiceListenResultMsg{}
	}
}

// voiceUnlisten mutes the mic and sends an in-band turn_end signal
// to the daemon, which triggers the model to process the user's speech.
func (m *InboxModel) voiceUnlisten() tea.Cmd {
	return func() tea.Msg {
		if m.voice.recorder != nil {
			m.voice.recorder.Mute()
		}
		if err := sendTurnSignal(m.voice.wsConn, "turn_end"); err != nil {
			return voiceUnlistenResultMsg{err: err}
		}
		return voiceUnlistenResultMsg{}
	}
}

// sendTurnSignal sends a JSON turn signal over the WebSocket as a text message.
func sendTurnSignal(conn *websocket.Conn, signalType string) error {
	if conn == nil {
		return fmt.Errorf("voice: no WebSocket connection")
	}
	data, err := json.Marshal(map[string]string{"type": signalType})
	if err != nil {
		return fmt.Errorf("voice: marshal turn signal: %w", err)
	}
	return conn.Write(context.Background(), websocket.MessageText, data)
}

// cleanupVoice synchronously releases all voice resources. Safe to call
// multiple times or when voice is not active.
func (m *InboxModel) cleanupVoice() {
	if !m.voice.active {
		return
	}
	if m.voice.cancelAudio != nil {
		m.voice.cancelAudio()
	}
	if m.voice.recorder != nil {
		m.voice.recorder.Close()
	}
	if m.voice.player != nil {
		m.voice.player.Close()
	}
	if m.voice.wsConn != nil {
		m.voice.wsConn.CloseNow()
	}
	// Best-effort stop on daemon side.
	if m.client != nil {
		_ = m.client.VoiceStop(context.Background())
	}

	m.voice = voiceState{}
}

// handleVoiceEvent updates voice state from SSE events. Called from the
// event handling path in both inbox and session views.
func (m *InboxModel) handleVoiceEvent(evt agent.Event) {
	switch evt.Type {
	case agent.EventVoiceStatus:
		if data, ok := evt.Data.(agent.VoiceStatusData); ok {
			switch data.Status {
			case agent.VoiceStatusListening:
				m.voice.speaking = false
			case agent.VoiceStatusSpeaking:
				m.voice.speaking = true
			case agent.VoiceStatusThinking:
				m.voice.speaking = false
			case agent.VoiceStatusIdle:
				m.voice.speaking = false
			}
		}
	// EventVoiceTranscript and EventVoiceToolCall are intentionally
	// ignored for now — no transcript UX yet.
	case agent.EventVoiceTranscript:
	case agent.EventVoiceToolCall:
	}
}

// voiceInputBlocked reports whether SPACE should be treated as a normal
// character (e.g. the user is typing text) rather than a voice trigger.
func (m *InboxModel) voiceInputBlocked() bool {
	// Inbox-level modal states.
	if m.showHelp || m.showConfirm || m.showMenu || m.searching || m.showKittyWarning {
		return true
	}
	// Session view modal/input states.
	if m.screen == screenSession && m.sessionView != nil {
		sv := m.sessionView
		if sv.inputActive || sv.showHelp || sv.showConfirm || sv.showMenu || sv.pendingPerm != nil {
			return true
		}
	}
	return false
}

// --- Rendering helpers ---

// voiceHeaderBadge returns a styled badge string reflecting the current
// voice state, or "" when voice is inactive.
func voiceHeaderBadge(v voiceState) string {
	if !v.active && !v.starting {
		return ""
	}
	if v.recording {
		return lipgloss.NewStyle().Foreground(dangerColor).Bold(true).Render("[REC]")
	}
	if v.speaking {
		return lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("[VOICE]")
	}
	// Active but idle — show a subtle indicator so the user knows
	// the voice session is still connected.
	return lipgloss.NewStyle().Foreground(dimColor).Render("[voice]")
}

// voiceHelpItem returns the help bar fragment for voice, or "".
func voiceHelpItem(v voiceState) string {
	if v.recording {
		return "space: stop"
	}
	return "space: talk"
}

// overlayKittyWarning renders a centered popup explaining that push-to-talk
// requires a terminal with Kitty keyboard protocol support.
func (m *InboxModel) overlayKittyWarning(base string) string {
	var sb strings.Builder

	innerWidth := 50

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(warningColor).
		Width(innerWidth).
		Render("Push-to-Talk Unavailable")
	sb.WriteString(title)
	sb.WriteString("\n\n")

	msg := lipgloss.NewStyle().
		Foreground(textColor).
		Width(innerWidth).
		Render("Push-to-talk requires a terminal that supports " +
			"the Kitty keyboard protocol (key release events).")
	sb.WriteString(msg)
	sb.WriteString("\n\n")

	supported := lipgloss.NewStyle().
		Foreground(dimColor).
		Width(innerWidth).
		Render("Supported terminals: Kitty, WezTerm, Ghostty, " +
			"foot, Rio, iTerm2 (with Kitty keyboard mode enabled).")
	sb.WriteString(supported)
	sb.WriteString("\n\n")

	hint := lipgloss.NewStyle().
		Foreground(dimColor).
		Render("press any key to dismiss")
	sb.WriteString(hint)

	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(warningColor).
		Padding(1, 2).
		Render(sb.String())

	return overlayCenter(base, popup, m.width, m.height)
}

// --- Daemon SSE event subscription for voice ---

// subscribeVoiceEvents starts an SSE subscription filtered for voice
// events. Voice events are global (not per-session) so they arrive on
// any subscription. We reuse the session view's existing subscription
// when on the session screen; on the inbox screen we need our own.
//
// For simplicity, the current implementation relies on the inbox's
// periodic data refresh and the session view's SSE subscription to
// forward voice events. This avoids a second SSE connection.
//
// Voice status changes are delivered via sessionEventMsg, which the
// inbox Update() intercepts before delegating to the session view.

// isVoiceEvent reports whether the event is a voice-related SSE event.
func isVoiceEvent(evt agent.Event) bool {
	switch evt.Type {
	case agent.EventVoiceTranscript, agent.EventVoiceStatus, agent.EventVoiceToolCall:
		return true
	}
	return false
}

// voiceStartAndListen is a convenience that starts the voice session and
// immediately begins listening. Used on the first SPACE press.
func (m *InboxModel) voiceStartAndListen() tea.Cmd {
	return func() tea.Msg {
		// Run startVoice inline.
		startMsg := m.startVoice()().(voiceStartResultMsg)
		if startMsg.err != nil {
			return startMsg
		}
		// Now listen.
		listenMsg := m.voiceListen()().(voiceListenResultMsg)
		if listenMsg.err != nil {
			// Voice started but listen failed — still return the listen error.
			// Voice state is active, user can retry.
			return voiceListenResultMsg{err: listenMsg.err}
		}
		// Return a composite: start succeeded, listen succeeded.
		// We use voiceStartResultMsg so the Update handler sets active=true
		// and then we immediately set recording=true.
		return voiceStartAndListenResultMsg{}
	}
}

// voiceStartAndListenResultMsg signals that both startVoice and voiceListen
// completed successfully.
type voiceStartAndListenResultMsg struct{}

// handleVoiceMsg processes voice-related messages in InboxModel.Update.
// Returns (handled bool, model, cmd).
func (m *InboxModel) handleVoiceMsg(msg tea.Msg) (bool, tea.Model, tea.Cmd) {
	switch msg.(type) {
	case voiceStartResultMsg:
		vmsg := msg.(voiceStartResultMsg)
		m.voice.starting = false
		if vmsg.err != nil {
			// Revert optimistic recording flag from key press.
			m.voice.recording = false
			m.err = vmsg.err
			return true, m, nil
		}
		m.voice.active = true
		return true, m, nil

	case voiceStartAndListenResultMsg:
		m.voice.starting = false
		m.voice.active = true
		// recording was already set optimistically on key press.
		return true, m, nil

	case voiceStopResultMsg:
		m.voice = voiceState{}
		return true, m, nil

	case voiceListenResultMsg:
		vmsg := msg.(voiceListenResultMsg)
		if vmsg.err != nil {
			// Revert optimistic recording flag.
			m.voice.recording = false
			m.err = vmsg.err
			return true, m, nil
		}
		// recording was already set optimistically on key press.
		return true, m, nil

	case voiceUnlistenResultMsg:
		vmsg := msg.(voiceUnlistenResultMsg)
		if vmsg.err != nil {
			// Revert optimistic recording flag.
			m.voice.recording = true
			m.err = vmsg.err
			return true, m, nil
		}
		// recording was already cleared optimistically on key release.
		return true, m, nil

	case voiceAudioErrMsg:
		vmsg := msg.(voiceAudioErrMsg)
		m.err = vmsg.err
		m.cleanupVoice()
		return true, m, nil
	}
	return false, m, nil
}

// handleVoiceKeyPress handles SPACE key press for push-to-talk.
// Returns (handled bool, cmd).
func (m *InboxModel) handleVoiceKeyPress(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if msg.String() != "space" {
		return false, nil
	}
	// Suppress key repeats from holding SPACE. The Kitty keyboard
	// protocol reports these with IsRepeat=true.
	if msg.IsRepeat {
		return true, nil
	}
	if m.voiceInputBlocked() {
		return false, nil
	}

	// Push-to-talk requires the Kitty keyboard protocol for
	// KeyReleaseMsg. Without it, show a one-time warning popup.
	if !m.kittyKeyboard {
		m.showKittyWarning = true
		return true, nil
	}

	// Guard against concurrent startup: if a voiceStartAndListen
	// command is already in flight, absorb the press.
	if m.voice.starting {
		return true, nil
	}

	if !m.voice.active {
		// First press: start voice session + begin listening.
		// Set recording optimistically so the UI reflects it immediately.
		m.voice.starting = true
		m.voice.recording = true
		return true, m.voiceStartAndListen()
	}
	if !m.voice.recording {
		// Session active but not recording: resume listening.
		// Set recording optimistically; reverted on error.
		m.voice.recording = true
		return true, m.voiceListen()
	}
	// Already recording — absorb. On Kitty terminals the release
	// event stops recording; this branch is unreachable in practice
	// because IsRepeat filters held-key repeats above.
	return true, nil
}

// handleVoiceKeyRelease handles SPACE key release for push-to-talk.
// Returns (handled bool, cmd).
func (m *InboxModel) handleVoiceKeyRelease(msg tea.KeyReleaseMsg) (bool, tea.Cmd) {
	if msg.String() != "space" {
		return false, nil
	}
	if !m.voice.active || !m.voice.recording {
		return false, nil
	}
	// Set recording=false optimistically; reverted on error.
	m.voice.recording = false
	return true, m.voiceUnlisten()
}

// handleVoiceSSE checks if a sessionEventMsg contains a voice event and
// handles it. Returns true if the event was consumed.
func (m *InboxModel) handleVoiceSSE(msg sessionEventMsg) bool {
	if isVoiceEvent(msg.event) {
		m.handleVoiceEvent(msg.event)
		return true
	}
	return false
}

// newVoiceEnabledView wraps tea.NewView and enables the keyboard
// enhancements needed for push-to-talk (KeyReleaseMsg).
func newVoiceEnabledView(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	v.KeyboardEnhancements = tea.KeyboardEnhancements{
		ReportEventTypes: true,
	}
	return v
}

// newVoiceEnabledViewWithMouse is like newVoiceEnabledView but also
// enables cell-motion mouse mode (used by the session view).
func newVoiceEnabledViewWithMouse(content string) tea.View {
	v := newVoiceEnabledView(content)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// voiceCleanupOnQuit returns a tea.Cmd that cleans up voice before
// sending tea.Quit.
func (m *InboxModel) voiceCleanupOnQuit() tea.Cmd {
	return func() tea.Msg {
		m.cleanupVoice()
		return tea.QuitMsg{}
	}
}

// passVoiceState gives a SessionViewModel read access to the current
// voice state for rendering purposes (header badge, help bar).
func (m *InboxModel) passVoiceState() {
	if m.sessionView != nil {
		m.sessionView.voice = &m.voice
	}
}

// ensureVoiceEventSubscription makes sure voice SSE events reach the
// inbox model. When on the session screen, the session view's SSE
// subscription already delivers all events (including voice). When on
// the inbox screen, we rely on voice events being handled when the
// session view forwards them via sessionEventMsg, or when the inbox
// has its own subscription (if we add one in the future).
//
// For now, voice events piggyback on the session view's SSE channel.
// This means voice indicators won't update on the inbox screen unless
// a session is open. This is acceptable for v1 since voice is most
// useful while viewing a session.
func ensureVoiceEventSubscription(_ *daemon.Client) {
	// Placeholder for future inbox-level SSE subscription.
}
