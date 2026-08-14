package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
)

func pickerSession(id, title string, updatedAgo time.Duration) agent.SessionInfo {
	return agent.SessionInfo{
		ID:        id,
		Backend:   agent.BackendOpenCode,
		Title:     title,
		UpdatedAt: time.Now().Add(-updatedAgo),
	}
}

// The whole point of the picker's ordering: most recent activity first,
// regardless of catalog order, with done/archived sessions gone.
func TestVisibleSessionsByActivity_SortsNewestFirstAndDropsHidden(t *testing.T) {
	t.Parallel()

	sessions := []agent.SessionInfo{
		pickerSession("old", "old", 3*time.Hour),
		{ID: "archived", UpdatedAt: time.Now(), Visibility: agent.VisibilityArchived},
		pickerSession("newest", "newest", time.Minute),
		{ID: "done", UpdatedAt: time.Now(), Visibility: agent.VisibilityDone},
		pickerSession("mid", "mid", time.Hour),
	}

	got := visibleSessionsByActivity(sessions)
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if want := "newest,mid,old"; strings.Join(ids, ",") != want {
		t.Errorf("order = %v, want %s", ids, want)
	}
}

func TestSessionPickerTitle_FallsBackToPromptFirstLine(t *testing.T) {
	t.Parallel()

	if got := sessionPickerTitle(agent.SessionInfo{Title: "Fix header"}); got != "Fix header" {
		t.Errorf("title: got %q", got)
	}
	if got := sessionPickerTitle(agent.SessionInfo{Prompt: "make the button blue\nand round"}); got != "make the button blue" {
		t.Errorf("prompt fallback: got %q", got)
	}
	if got := sessionPickerTitle(agent.SessionInfo{}); got != "(untitled)" {
		t.Errorf("placeholder: got %q", got)
	}
}

// loadPicker builds a picker and feeds it a loaded catalog, as Init's
// loadCmd would.
func loadPicker(t *testing.T, sessions []agent.SessionInfo) *SessionPickerModel {
	t.Helper()
	m := NewSessionPickerModel(nil, t.TempDir())
	updated, _ := m.Update(sessionPickerLoadedMsg{sessions: sessions})
	picker, ok := updated.(*SessionPickerModel)
	if !ok {
		t.Fatalf("Update returned %T", updated)
	}
	return picker
}

// The rediscover action is anchored first for visibility, but the
// cursor starts on the newest session — plain enter must attach, not
// rediscover.
func TestSessionPicker_EnterSelectsNewestSession(t *testing.T) {
	t.Parallel()

	m := loadPicker(t, []agent.SessionInfo{
		pickerSession("older", "older work", time.Hour),
		pickerSession("newest", "latest work", time.Minute),
	})

	if m.filtered[0].index != sessionPickerIndexRediscover {
		t.Error("rediscover action not anchored first")
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a session row produced no command (want quit)")
	}
	result := updated.(*SessionPickerModel).Result()
	if result.SessionID != "newest" {
		t.Errorf("SessionID = %q, want the newest session preselected", result.SessionID)
	}
	if result.IsAborted {
		t.Error("a selection must not report an abort")
	}
}

func TestSessionPicker_EscClearsFilterThenCancels(t *testing.T) {
	t.Parallel()

	m := loadPicker(t, []agent.SessionInfo{pickerSession("a", "a", time.Minute)})
	m.search.SetValue("query")
	m.lastQuery = "query"
	m.applyFilter()

	// First esc backs out of the typed filter without quitting.
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	picker := updated.(*SessionPickerModel)
	if cmd != nil {
		t.Error("esc with a typed filter must not quit")
	}
	if picker.search.Value() != "" {
		t.Errorf("filter = %q, want cleared", picker.search.Value())
	}

	// Second esc (empty input) cancels the run, same as ctrl+c.
	updated, _ = picker.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if result := updated.(*SessionPickerModel).Result(); !result.IsAborted {
		t.Errorf("bare esc: result = %+v, want abort", result)
	}
}

// Wheel events move the cursor, damped: trackpads emit event bursts, so
// one cursor step takes sessionPickerWheelDivisor same-direction events
// and a direction change resets the count.
func TestSessionPicker_MouseWheelMovesCursorDamped(t *testing.T) {
	t.Parallel()

	m := loadPicker(t, []agent.SessionInfo{
		pickerSession("a", "a", time.Minute),
		pickerSession("b", "b", time.Hour),
	})
	start := m.cursor

	wheel := func(button tea.MouseButton, times int) {
		for range times {
			updated, _ := m.Update(tea.MouseWheelMsg{Button: button})
			m = updated.(*SessionPickerModel)
		}
	}

	wheel(tea.MouseWheelDown, sessionPickerWheelDivisor-1)
	if m.cursor != start {
		t.Errorf("cursor moved after %d events, want none before the divisor", sessionPickerWheelDivisor-1)
	}
	wheel(tea.MouseWheelDown, 1)
	if m.cursor != start+1 {
		t.Errorf("cursor = %d after %d wheel-down events, want %d", m.cursor, sessionPickerWheelDivisor, start+1)
	}

	// A direction change must not inherit the previous burst's progress.
	wheel(tea.MouseWheelDown, sessionPickerWheelDivisor-1)
	wheel(tea.MouseWheelUp, sessionPickerWheelDivisor-1)
	if m.cursor != start+1 {
		t.Errorf("cursor = %d mid-burst after direction change, want unchanged %d", m.cursor, start+1)
	}
	wheel(tea.MouseWheelUp, 1)
	if m.cursor != start {
		t.Errorf("wheel up: cursor = %d, want back at %d", m.cursor, start)
	}
}

func TestSessionPicker_CtrlCAborts(t *testing.T) {
	t.Parallel()

	m := loadPicker(t, []agent.SessionInfo{pickerSession("a", "a", time.Minute)})
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if result := updated.(*SessionPickerModel).Result(); !result.IsAborted {
		t.Errorf("ctrl+c: result = %+v, want abort", result)
	}
}

// The rediscover action must survive any filter query — it exists
// precisely for sessions the query can't find.
func TestSessionPicker_FilterKeepsRediscoverRowFirst(t *testing.T) {
	t.Parallel()

	m := loadPicker(t, []agent.SessionInfo{
		pickerSession("a", "fix the header", time.Minute),
		pickerSession("b", "dark mode", time.Hour),
	})

	m.search.SetValue("header")
	m.lastQuery = "header"
	m.applyFilter()

	if len(m.filtered) != 2 {
		t.Fatalf("filtered rows = %d, want the rediscover action plus the match", len(m.filtered))
	}
	if m.filtered[0].index != sessionPickerIndexRediscover {
		t.Error("rediscover action not anchored first under a filter")
	}
	if m.filtered[1].index < 0 || m.sessions[m.filtered[1].index].ID != "a" {
		t.Errorf("second row = %+v, want the matching session", m.filtered[1])
	}
}

func TestSessionPicker_EnterOnRediscoverRowStartsDiscovery(t *testing.T) {
	t.Parallel()

	m := loadPicker(t, nil) // empty catalog: the action row is the only one

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	picker := updated.(*SessionPickerModel)
	if picker.phase != sessionPickerDiscovering {
		t.Errorf("phase = %v, want discovering", picker.phase)
	}
	if cmd == nil {
		t.Error("rediscover selection produced no command")
	}
}

// A discovery outcome reports its delta and triggers a reload rather
// than trusting its own count of the catalog.
func TestSessionPicker_DiscoveredMsgReloads(t *testing.T) {
	t.Parallel()

	m := loadPicker(t, nil)
	updated, cmd := m.Update(sessionPickerDiscoveredMsg{imported: 2})
	picker := updated.(*SessionPickerModel)
	if !strings.Contains(picker.notice, "2 new sessions") {
		t.Errorf("notice = %q, want the import delta", picker.notice)
	}
	if cmd == nil {
		t.Error("discovery outcome did not schedule a reload")
	}
}
