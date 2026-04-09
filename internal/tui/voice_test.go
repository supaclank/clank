package tui

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/acksell/clank/internal/agent"
)

// --- voiceHeaderBadge ---

func TestVoiceHeaderBadge_Inactive(t *testing.T) {
	t.Parallel()
	badge := voiceHeaderBadge(voiceState{active: false})
	if badge != "" {
		t.Errorf("expected empty badge for inactive voice, got %q", badge)
	}
}

func TestVoiceHeaderBadge_StartingShowsREC(t *testing.T) {
	t.Parallel()
	// During first-press startup, active is still false but starting
	// and recording are set optimistically. The badge should show [REC]
	// immediately so the UI feels instant.
	badge := voiceHeaderBadge(voiceState{active: false, starting: true, recording: true})
	if badge == "" {
		t.Fatal("expected non-empty badge during startup")
	}
	if !containsPlainText(badge, "REC") {
		t.Errorf("expected badge to contain 'REC' during startup, got %q", badge)
	}
}

func TestVoiceHeaderBadge_Recording(t *testing.T) {
	t.Parallel()
	badge := voiceHeaderBadge(voiceState{active: true, recording: true})
	if badge == "" {
		t.Fatal("expected non-empty badge when recording")
	}
	// Should contain "REC" visually.
	if !containsPlainText(badge, "REC") {
		t.Errorf("expected badge to contain 'REC', got %q", badge)
	}
}

func TestVoiceHeaderBadge_Speaking(t *testing.T) {
	t.Parallel()
	badge := voiceHeaderBadge(voiceState{active: true, speaking: true})
	if badge == "" {
		t.Fatal("expected non-empty badge when speaking")
	}
	if !containsPlainText(badge, "VOICE") {
		t.Errorf("expected badge to contain 'VOICE', got %q", badge)
	}
}

func TestVoiceHeaderBadge_ActiveIdle(t *testing.T) {
	t.Parallel()
	badge := voiceHeaderBadge(voiceState{active: true})
	if badge == "" {
		t.Fatal("expected non-empty badge when active but idle")
	}
	if !containsPlainText(badge, "voice") {
		t.Errorf("expected badge to contain 'voice', got %q", badge)
	}
}

// --- voiceHelpItem ---

func TestVoiceHelpItem_NotRecording(t *testing.T) {
	t.Parallel()
	item := voiceHelpItem(voiceState{})
	if item != "space: talk" {
		t.Errorf("expected 'space: talk', got %q", item)
	}
}

func TestVoiceHelpItem_Recording(t *testing.T) {
	t.Parallel()
	item := voiceHelpItem(voiceState{recording: true})
	if item != "space: stop" {
		t.Errorf("expected 'space: stop', got %q", item)
	}
}

// --- isVoiceEvent ---

func TestIsVoiceEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		eventType agent.EventType
		want      bool
	}{
		{agent.EventVoiceTranscript, true},
		{agent.EventVoiceStatus, true},
		{agent.EventVoiceToolCall, true},
		{agent.EventStatusChange, false},
		{agent.EventMessage, false},
		{agent.EventError, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.eventType), func(t *testing.T) {
			t.Parallel()
			got := isVoiceEvent(agent.Event{Type: tt.eventType})
			if got != tt.want {
				t.Errorf("isVoiceEvent(%q) = %v, want %v", tt.eventType, got, tt.want)
			}
		})
	}
}

// --- voiceInputBlocked ---

func TestVoiceInputBlocked_InboxDefaults(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	if m.voiceInputBlocked() {
		t.Error("expected voice input NOT blocked with default inbox state")
	}
}

func TestVoiceInputBlocked_InboxHelp(t *testing.T) {
	t.Parallel()
	m := &InboxModel{showHelp: true}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when help overlay is open")
	}
}

func TestVoiceInputBlocked_InboxConfirm(t *testing.T) {
	t.Parallel()
	m := &InboxModel{showConfirm: true}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when confirm dialog is open")
	}
}

func TestVoiceInputBlocked_InboxMenu(t *testing.T) {
	t.Parallel()
	m := &InboxModel{showMenu: true}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when menu is open")
	}
}

func TestVoiceInputBlocked_InboxSearching(t *testing.T) {
	t.Parallel()
	m := &InboxModel{searching: true}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when searching")
	}
}

func TestVoiceInputBlocked_SessionInputActive(t *testing.T) {
	t.Parallel()
	m := &InboxModel{
		screen:      screenSession,
		sessionView: &SessionViewModel{inputActive: true},
	}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when session input is active")
	}
}

func TestVoiceInputBlocked_SessionHelp(t *testing.T) {
	t.Parallel()
	m := &InboxModel{
		screen:      screenSession,
		sessionView: &SessionViewModel{showHelp: true},
	}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when session help overlay is open")
	}
}

func TestVoiceInputBlocked_SessionPermission(t *testing.T) {
	t.Parallel()
	m := &InboxModel{
		screen:      screenSession,
		sessionView: &SessionViewModel{pendingPerm: &agent.PermissionData{}},
	}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when session has pending permission")
	}
}

func TestVoiceInputBlocked_SessionNormalMode(t *testing.T) {
	t.Parallel()
	m := &InboxModel{
		screen:      screenSession,
		sessionView: &SessionViewModel{},
	}
	if m.voiceInputBlocked() {
		t.Error("expected voice input NOT blocked in session normal mode")
	}
}

// --- handleVoiceMsg ---

func TestHandleVoiceMsg_StartSuccess(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.starting = true
	handled, _, _ := m.handleVoiceMsg(voiceStartResultMsg{})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if !m.voice.active {
		t.Error("expected voice.active to be true after successful start")
	}
	if m.voice.starting {
		t.Error("expected voice.starting to be cleared after start result")
	}
}

func TestHandleVoiceMsg_StartError(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.starting = true
	m.voice.recording = true // set optimistically by key press
	handled, _, _ := m.handleVoiceMsg(voiceStartResultMsg{err: errVoiceTest})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if m.voice.active {
		t.Error("expected voice.active to be false after failed start")
	}
	if m.voice.recording {
		t.Error("expected voice.recording to be reverted to false after failed start")
	}
	if m.err == nil {
		t.Error("expected error to be set")
	}
	if m.voice.starting {
		t.Error("expected voice.starting to be cleared after failed start")
	}
}

func TestHandleVoiceMsg_StartAndListen(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.starting = true
	m.voice.recording = true // set optimistically by key press
	handled, _, _ := m.handleVoiceMsg(voiceStartAndListenResultMsg{})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if !m.voice.active {
		t.Error("expected voice.active to be true")
	}
	if !m.voice.recording {
		t.Error("expected voice.recording to remain true")
	}
	if m.voice.starting {
		t.Error("expected voice.starting to be cleared after startAndListen result")
	}
}

func TestHandleVoiceMsg_ListenSuccess(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = true // set optimistically by key press
	handled, _, _ := m.handleVoiceMsg(voiceListenResultMsg{})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if !m.voice.recording {
		t.Error("expected voice.recording to remain true after listen confirmation")
	}
}

func TestHandleVoiceMsg_UnlistenSuccess(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = false // cleared optimistically by key release
	handled, _, _ := m.handleVoiceMsg(voiceUnlistenResultMsg{})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if m.voice.recording {
		t.Error("expected voice.recording to remain false after unlisten confirmation")
	}
}

func TestHandleVoiceMsg_Stop(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = true
	handled, _, _ := m.handleVoiceMsg(voiceStopResultMsg{})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if m.voice.active {
		t.Error("expected voice.active to be false after stop")
	}
}

func TestHandleVoiceMsg_AudioErr(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	handled, _, _ := m.handleVoiceMsg(voiceAudioErrMsg{err: errVoiceTest})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if m.voice.active {
		t.Error("expected voice.active to be false after audio error")
	}
	if m.err == nil {
		t.Error("expected error to be set")
	}
}

func TestHandleVoiceMsg_UnrelatedMessage(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	handled, _, _ := m.handleVoiceMsg(tea.WindowSizeMsg{Width: 80, Height: 24})
	if handled {
		t.Error("expected unrelated message NOT to be handled")
	}
}

// --- handleVoiceKeyPress ---

func TestHandleVoiceKeyPress_NonSpace(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	handled, _ := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: "m", Code: 'm'})
	if handled {
		t.Error("expected non-space key NOT to be handled")
	}
}

func TestHandleVoiceKeyPress_SpaceWhenBlocked(t *testing.T) {
	t.Parallel()
	m := &InboxModel{showHelp: true}
	handled, _ := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace})
	if handled {
		t.Error("expected space NOT to be handled when input is blocked")
	}
}

func TestHandleVoiceKeyPress_SpaceFirstPress(t *testing.T) {
	t.Parallel()
	m := &InboxModel{kittyKeyboard: true}
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace})
	if !handled {
		t.Fatal("expected space to be handled")
	}
	if cmd == nil {
		t.Error("expected a command to be returned for first press (startAndListen)")
	}
	if !m.voice.starting {
		t.Error("expected voice.starting to be true after first press")
	}
	if !m.voice.recording {
		t.Error("expected voice.recording to be true immediately (optimistic)")
	}
}

func TestHandleVoiceKeyPress_SpaceResumeRecording(t *testing.T) {
	t.Parallel()
	m := &InboxModel{kittyKeyboard: true}
	m.voice.active = true
	m.voice.recording = false
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace})
	if !handled {
		t.Fatal("expected space to be handled")
	}
	if cmd == nil {
		t.Error("expected a command to be returned for resume recording")
	}
	if !m.voice.recording {
		t.Error("expected voice.recording to be true immediately (optimistic)")
	}
}

func TestHandleVoiceKeyPress_SpaceWhileRecordingIsNoop(t *testing.T) {
	t.Parallel()
	// When already recording, a second press is absorbed as a no-op.
	// This prevents the rapid listen/unlisten cycling bug on terminals
	// that don't properly report IsRepeat.
	m := &InboxModel{kittyKeyboard: true}
	m.voice.active = true
	m.voice.recording = true
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace})
	if !handled {
		t.Fatal("expected space to be handled")
	}
	if cmd != nil {
		t.Error("expected no command when already recording (no-op)")
	}
}

func TestHandleVoiceKeyPress_RepeatIgnored(t *testing.T) {
	t.Parallel()
	// Key repeats from holding SPACE must be absorbed without
	// triggering any state change or command.
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = true
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace, IsRepeat: true})
	if !handled {
		t.Fatal("expected repeat space to be handled (absorbed)")
	}
	if cmd != nil {
		t.Error("expected no command for key repeat")
	}
}

func TestHandleVoiceKeyPress_RepeatIgnoredWhenInactive(t *testing.T) {
	t.Parallel()
	// Even when voice is inactive, repeats should not start a session.
	m := &InboxModel{}
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace, IsRepeat: true})
	if !handled {
		t.Fatal("expected repeat space to be handled (absorbed)")
	}
	if cmd != nil {
		t.Error("expected no command for key repeat")
	}
	if m.voice.starting {
		t.Error("expected voice.starting to remain false for repeat")
	}
}

func TestHandleVoiceKeyPress_StartingGuard(t *testing.T) {
	t.Parallel()
	// While a voiceStartAndListen command is in flight, additional
	// presses must be absorbed to prevent concurrent starts.
	m := &InboxModel{kittyKeyboard: true}
	m.voice.starting = true
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace})
	if !handled {
		t.Fatal("expected space to be handled (absorbed by starting guard)")
	}
	if cmd != nil {
		t.Error("expected no command while starting is in flight")
	}
}

// --- handleVoiceKeyRelease ---

func TestHandleVoiceKeyRelease_NonSpace(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = true
	handled, _ := m.handleVoiceKeyRelease(tea.KeyReleaseMsg{Text: "m", Code: 'm'})
	if handled {
		t.Error("expected non-space key release NOT to be handled")
	}
}

func TestHandleVoiceKeyRelease_SpaceNotRecording(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = false
	handled, _ := m.handleVoiceKeyRelease(tea.KeyReleaseMsg{Text: " ", Code: tea.KeySpace})
	if handled {
		t.Error("expected space release NOT to be handled when not recording")
	}
}

func TestHandleVoiceKeyRelease_SpaceWhileRecording(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = true
	handled, cmd := m.handleVoiceKeyRelease(tea.KeyReleaseMsg{Text: " ", Code: tea.KeySpace})
	if !handled {
		t.Fatal("expected space release to be handled when recording")
	}
	if cmd == nil {
		t.Error("expected a command (unlisten) on space release")
	}
	if m.voice.recording {
		t.Error("expected voice.recording to be false immediately (optimistic)")
	}
}

func TestHandleVoiceKeyRelease_SpaceWhenVoiceInactive(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	handled, _ := m.handleVoiceKeyRelease(tea.KeyReleaseMsg{Text: " ", Code: tea.KeySpace})
	if handled {
		t.Error("expected space release NOT to be handled when voice is inactive")
	}
}

// --- Kitty keyboard detection ---

func TestHandleVoiceKeyPress_NoKittyShowsWarning(t *testing.T) {
	t.Parallel()
	// Without Kitty keyboard support, pressing SPACE should show
	// the warning popup instead of starting voice.
	m := &InboxModel{kittyKeyboard: false}
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace})
	if !handled {
		t.Fatal("expected space to be handled")
	}
	if cmd != nil {
		t.Error("expected no command (voice should not start)")
	}
	if !m.showKittyWarning {
		t.Error("expected showKittyWarning to be true")
	}
	if m.voice.starting {
		t.Error("expected voice.starting to remain false")
	}
}

func TestHandleVoiceKeyPress_KittyStartsVoice(t *testing.T) {
	t.Parallel()
	// With Kitty keyboard support, pressing SPACE starts voice normally.
	m := &InboxModel{kittyKeyboard: true}
	handled, cmd := m.handleVoiceKeyPress(tea.KeyPressMsg{Text: " ", Code: tea.KeySpace})
	if !handled {
		t.Fatal("expected space to be handled")
	}
	if cmd == nil {
		t.Error("expected a command for voice start")
	}
	if !m.voice.starting {
		t.Error("expected voice.starting to be true")
	}
}

// --- Optimistic state revert on error ---

func TestHandleVoiceMsg_ListenError_RevertsRecording(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = true // set optimistically
	handled, _, _ := m.handleVoiceMsg(voiceListenResultMsg{err: errVoiceTest})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if m.voice.recording {
		t.Error("expected voice.recording to be reverted to false on listen error")
	}
	if m.err == nil {
		t.Error("expected error to be set")
	}
}

func TestHandleVoiceMsg_UnlistenError_RevertsRecording(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.recording = false // cleared optimistically
	handled, _, _ := m.handleVoiceMsg(voiceUnlistenResultMsg{err: errVoiceTest})
	if !handled {
		t.Fatal("expected message to be handled")
	}
	if !m.voice.recording {
		t.Error("expected voice.recording to be reverted to true on unlisten error")
	}
	if m.err == nil {
		t.Error("expected error to be set")
	}
}

// --- voiceInputBlocked: Kitty warning ---

func TestVoiceInputBlocked_KittyWarning(t *testing.T) {
	t.Parallel()
	m := &InboxModel{showKittyWarning: true}
	if !m.voiceInputBlocked() {
		t.Error("expected voice input blocked when Kitty warning is shown")
	}
}

// --- handleVoiceEvent ---

func TestHandleVoiceEvent_StatusListening(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.speaking = true
	m.handleVoiceEvent(agent.Event{
		Type: agent.EventVoiceStatus,
		Data: agent.VoiceStatusData{Status: agent.VoiceStatusListening},
	})
	if m.voice.speaking {
		t.Error("expected speaking to be false after listening status")
	}
}

func TestHandleVoiceEvent_StatusSpeaking(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.handleVoiceEvent(agent.Event{
		Type: agent.EventVoiceStatus,
		Data: agent.VoiceStatusData{Status: agent.VoiceStatusSpeaking},
	})
	if !m.voice.speaking {
		t.Error("expected speaking to be true after speaking status")
	}
}

func TestHandleVoiceEvent_StatusThinking(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.speaking = true
	m.handleVoiceEvent(agent.Event{
		Type: agent.EventVoiceStatus,
		Data: agent.VoiceStatusData{Status: agent.VoiceStatusThinking},
	})
	if m.voice.speaking {
		t.Error("expected speaking to be false after thinking status")
	}
}

func TestHandleVoiceEvent_StatusIdle(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	m.voice.speaking = true
	m.handleVoiceEvent(agent.Event{
		Type: agent.EventVoiceStatus,
		Data: agent.VoiceStatusData{Status: agent.VoiceStatusIdle},
	})
	if m.voice.speaking {
		t.Error("expected speaking to be false after idle status")
	}
}

// --- handleVoiceSSE ---

func TestHandleVoiceSSE_VoiceEvent(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	m.voice.active = true
	consumed := m.handleVoiceSSE(sessionEventMsg{
		event: agent.Event{
			Type: agent.EventVoiceStatus,
			Data: agent.VoiceStatusData{Status: agent.VoiceStatusSpeaking},
		},
	})
	if !consumed {
		t.Error("expected voice SSE event to be consumed")
	}
	if !m.voice.speaking {
		t.Error("expected speaking state to be updated")
	}
}

func TestHandleVoiceSSE_NonVoiceEvent(t *testing.T) {
	t.Parallel()
	m := &InboxModel{}
	consumed := m.handleVoiceSSE(sessionEventMsg{
		event: agent.Event{Type: agent.EventStatusChange},
	})
	if consumed {
		t.Error("expected non-voice SSE event NOT to be consumed")
	}
}

// --- cleanupVoice ---

func TestCleanupVoice_WhenInactive(t *testing.T) {
	t.Parallel()
	// Should not panic when voice is inactive.
	m := &InboxModel{}
	m.cleanupVoice()
	if m.voice.active {
		t.Error("expected voice to remain inactive")
	}
}

// --- helpers ---

var errVoiceTest = errors.New("test error")

// containsPlainText checks if s contains target, stripping ANSI codes.
func containsPlainText(s, target string) bool {
	// Strip ANSI escape sequences for comparison.
	clean := stripANSI(s)
	return voiceContains(clean, target)
}

func voiceContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	var result []byte
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			// Skip escape sequence.
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
					i++
				}
				if i < len(s) {
					i++ // skip the final letter
				}
			}
		} else {
			result = append(result, s[i])
			i++
		}
	}
	return string(result)
}
