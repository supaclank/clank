package clankcli

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/clanksync/triggers"
)

func updateKey(m harnessSelectModel, code rune) harnessSelectModel {
	next, _ := m.Update(tea.KeyPressMsg{Code: code})
	return next.(harnessSelectModel)
}

func TestHarnessSelect_DefaultBothOnEnter(t *testing.T) {
	t.Parallel()
	m := updateKey(newHarnessSelectModel(), tea.KeyEnter)
	if !m.done || m.canceled {
		t.Fatalf("enter should confirm: done=%v canceled=%v", m.done, m.canceled)
	}
	got := m.selectedValues()
	if strings.Join(got, ",") != triggers.HarnessClaudeCode+","+triggers.HarnessOpenCode {
		t.Fatalf("default selection should be both, got %v", got)
	}
}

func TestHarnessSelect_SpaceTogglesCurrent(t *testing.T) {
	t.Parallel()
	// Cursor starts on Claude (index 0); space toggles it off, leaving opencode.
	m := updateKey(newHarnessSelectModel(), tea.KeySpace)
	m = updateKey(m, tea.KeyEnter)
	got := m.selectedValues()
	if len(got) != 1 || got[0] != triggers.HarnessOpenCode {
		t.Fatalf("after toggling Claude off, want [opencode], got %v", got)
	}
}

func TestHarnessSelect_NavigateThenToggle(t *testing.T) {
	t.Parallel()
	// Move to opencode (index 1) and toggle it off, leaving Claude.
	m := updateKey(newHarnessSelectModel(), tea.KeyDown)
	m = updateKey(m, tea.KeySpace)
	m = updateKey(m, tea.KeyEnter)
	got := m.selectedValues()
	if len(got) != 1 || got[0] != triggers.HarnessClaudeCode {
		t.Fatalf("after toggling opencode off, want [claude-code], got %v", got)
	}
}

func TestHarnessSelect_EnterWithNoneSelectedIsRefused(t *testing.T) {
	t.Parallel()
	// Toggle both off, then Enter must NOT confirm (autopush needs ≥1).
	m := updateKey(newHarnessSelectModel(), tea.KeySpace) // Claude off
	m = updateKey(m, tea.KeyDown)
	m = updateKey(m, tea.KeySpace) // opencode off
	m = updateKey(m, tea.KeyEnter)
	if m.done {
		t.Fatal("enter with nothing selected must not confirm")
	}
}

func TestHarnessSelect_EscCancels(t *testing.T) {
	t.Parallel()
	m := updateKey(newHarnessSelectModel(), tea.KeyEsc)
	if !m.canceled {
		t.Fatal("esc should cancel")
	}
}
