package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestNormalizeKeyCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		msg      tea.KeyPressMsg
		wantText string
		wantStr  string
	}{
		{
			name:     "lowercase letter unchanged",
			msg:      tea.KeyPressMsg{Text: "m", Code: 'm'},
			wantText: "m",
			wantStr:  "m",
		},
		{
			name:     "caps lock uppercase normalized to lowercase",
			msg:      tea.KeyPressMsg{Text: "M", Code: 'm'},
			wantText: "m",
			wantStr:  "m",
		},
		{
			name:     "shift+letter preserved as uppercase",
			msg:      tea.KeyPressMsg{Text: "G", Code: 'g', Mod: tea.ModShift},
			wantText: "G",
			wantStr:  "G",
		},
		{
			name:     "caps lock G without shift normalized to lowercase",
			msg:      tea.KeyPressMsg{Text: "G", Code: 'g'},
			wantText: "g",
			wantStr:  "g",
		},
		{
			name:     "ctrl+c unaffected (no text)",
			msg:      tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
			wantText: "",
			wantStr:  "ctrl+c",
		},
		{
			name:     "non-letter key unaffected",
			msg:      tea.KeyPressMsg{Code: tea.KeyEnter},
			wantText: "",
			wantStr:  "enter",
		},
		{
			name:     "number key unaffected",
			msg:      tea.KeyPressMsg{Text: "1", Code: '1'},
			wantText: "1",
			wantStr:  "1",
		},
		{
			name:     "caps lock y (permission prompt)",
			msg:      tea.KeyPressMsg{Text: "Y", Code: 'y'},
			wantText: "y",
			wantStr:  "y",
		},
		{
			name:     "caps lock q (quit shortcut)",
			msg:      tea.KeyPressMsg{Text: "Q", Code: 'q'},
			wantText: "q",
			wantStr:  "q",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeKeyCase(tt.msg)
			if got.Text != tt.wantText {
				t.Errorf("normalizeKeyCase() Text = %q, want %q", got.Text, tt.wantText)
			}
			if got.String() != tt.wantStr {
				t.Errorf("normalizeKeyCase() String() = %q, want %q", got.String(), tt.wantStr)
			}
		})
	}
}
