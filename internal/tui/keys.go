package tui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// normalizeKeyCase returns a tea.KeyPressMsg with single-letter Text
// lowercased when the shift modifier is not explicitly held. This makes
// shortcuts like "m" work regardless of caps-lock state: caps-lock
// produces Text="M" without the shift modifier, whereas an intentional
// Shift+m sets the shift modifier.
func normalizeKeyCase(msg tea.KeyPressMsg) tea.KeyPressMsg {
	if msg.Mod.Contains(tea.ModShift) {
		return msg
	}
	if len(msg.Text) > 0 {
		lower := strings.ToLower(msg.Text)
		if lower != msg.Text {
			msg.Text = lower
		}
	}
	// Also normalize ShiftedCode so that String() uses the lowered text.
	if msg.ShiftedCode != 0 && unicode.IsUpper(msg.ShiftedCode) {
		msg.ShiftedCode = unicode.ToLower(msg.ShiftedCode)
	}
	return msg
}
