package tui

// Voice support is removed in PR 3 (hub deletion). The TUI voice plumbing
// is preserved as a no-op so existing key bindings and UI hooks compile.
// Voice will be re-introduced in a future PR once it has a cleaner home
// outside the hub.

import (
	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// voiceState is an empty placeholder for the former TUI voice session
// state.
type voiceState struct{}

// Bubble Tea message stubs retained so call sites continue to compile.
type voiceStartResultMsg struct{ err error }
type voiceStopResultMsg struct{ err error }
type voiceListenResultMsg struct{ err error }
type voiceUnlistenResultMsg struct{ err error }
type voiceAudioErrMsg struct{ err error }
type voiceStartAndListenResultMsg struct{}

func (m *InboxModel) startVoice() tea.Cmd                                            { return nil }
func (m *InboxModel) stopVoice() tea.Cmd                                             { return nil }
func (m *InboxModel) voiceListen() tea.Cmd                                           { return nil }
func (m *InboxModel) voiceUnlisten() tea.Cmd                                         { return nil }
func (m *InboxModel) cleanupVoice()                                                  {}
func (m *InboxModel) handleVoiceEvent(_ agent.Event)                                 {}
func (m *InboxModel) voiceInputBlocked() bool                                        { return false }
func (m *InboxModel) voiceStartAndListen() tea.Cmd                                   { return nil }
func (m *InboxModel) handleVoiceMsg(tea.Msg) (bool, tea.Model, tea.Cmd)              { return false, m, nil }
func (m *InboxModel) handleVoiceKeyPress(tea.KeyPressMsg) (bool, tea.Cmd)            { return false, nil }
func (m *InboxModel) handleVoiceKeyRelease(tea.KeyReleaseMsg) (bool, tea.Cmd)        { return false, nil }
func (m *InboxModel) handleVoiceSSE(sessionEventMsg) bool                            { return false }
func (m *InboxModel) overlayKittyWarning(base string) string                         { return base }
func (m *InboxModel) voiceCleanupOnQuit() tea.Cmd                                    { return nil }
func (m *InboxModel) passVoiceState()                                                {}

func voiceHeaderBadge(_ voiceState) string { return "" }
func voiceHelpItem(_ voiceState) string    { return "" }
func isVoiceEvent(_ agent.Event) bool      { return false }

// newVoiceEnabledView (legacy name) builds the standard inbox/session
// View with AltScreen turned on so the TUI restores the user's terminal
// buffer on exit. The "voice-enabled" naming is historical — voice was
// removed in PR 3 — but the alt-screen + keyboard-enhancements wiring
// is what kept the inbox feel right and is preserved here.
func newVoiceEnabledView(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	v.KeyboardEnhancements = tea.KeyboardEnhancements{ReportEventTypes: true}
	return v
}

// newVoiceEnabledViewWithMouse is like newVoiceEnabledView but also
// enables cell-motion mouse mode (used by the session view).
func newVoiceEnabledViewWithMouse(content string) tea.View {
	v := newVoiceEnabledView(content)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func ensureVoiceEventSubscription(_ *daemonclient.Client) {}
