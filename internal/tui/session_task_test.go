package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
)

func TestSessionTaskViewIsInlineAndHidesInitialPrompt(t *testing.T) {
	t.Parallel()

	m := newTestSessionTask(t)
	m.session.info = &agent.SessionInfo{Status: agent.StatusBusy}
	m.session.historyLoaded = true
	m.session.entries = []displayEntry{
		{kind: entryUser, content: "very large setup prompt"},
		{kind: entryTool, content: "Read package.json"},
	}
	_, _ = m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})

	view := m.View()
	if view.AltScreen {
		t.Fatal("task view unexpectedly uses the alternate screen")
	}
	if view.MouseMode != tea.MouseModeNone {
		t.Fatalf("task view mouse mode = %v", view.MouseMode)
	}
	if !strings.Contains(view.Content, "One-time preview setup") || !strings.Contains(view.Content, "Read package.json") {
		t.Fatalf("task view missing task progress:\n%s", view.Content)
	}
	if strings.Contains(view.Content, "very large setup prompt") {
		t.Fatalf("task view exposes the setup prompt:\n%s", view.Content)
	}
}

func TestSessionTaskDoesNotOpenComposer(t *testing.T) {
	t.Parallel()

	m := newTestSessionTask(t)
	m.session.info = &agent.SessionInfo{Status: agent.StatusBusy}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	if m.session.inputActive {
		t.Fatal("task mode opened the chat composer")
	}
}

func TestSessionTaskDelegatesPermissionDecisions(t *testing.T) {
	t.Parallel()

	m := newTestSessionTask(t)
	m.session.info = &agent.SessionInfo{Status: agent.StatusBusy}
	m.session.pendingPerms = []agent.PermissionData{{RequestID: "permission-1", Tool: "write", Description: "write launch config"}}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("permission decision produced no command")
	}
	if m.session.replyingPermID != "permission-1" {
		t.Fatalf("replying permission = %q", m.session.replyingPermID)
	}
}

func TestSessionTaskExitsOnlyAfterInitialStateLoadsAndAgentSettles(t *testing.T) {
	t.Parallel()

	m := newTestSessionTask(t)
	idle := &agent.SessionInfo{
		ID:             "session-1",
		Status:         agent.StatusIdle,
		CurrentModeID:  "build",
		AvailableModes: []agent.SessionMode{{ID: "build", Name: "Build"}},
	}
	_, cmd := m.Update(sessionInfoMsg{info: idle})
	if cmd != nil {
		t.Fatal("task exited before history and permissions loaded")
	}
	_, cmd = m.Update(sessionMessagesMsg{})
	if cmd != nil {
		t.Fatal("task exited before pending permissions loaded")
	}
	_, cmd = m.Update(pendingPermissionMsg{})
	assertQuitCommand(t, cmd)
	if result := m.Result(); result.Status != agent.StatusIdle || result.Err != nil {
		t.Fatalf("Result = %+v", result)
	}
}

func TestSessionTaskExitsWhenPendingPermissionFetchFails(t *testing.T) {
	t.Parallel()

	m := newTestSessionTask(t)
	idle := &agent.SessionInfo{
		ID:             "session-1",
		Status:         agent.StatusIdle,
		CurrentModeID:  "build",
		AvailableModes: []agent.SessionMode{{ID: "build", Name: "Build"}},
	}
	_, _ = m.Update(sessionInfoMsg{info: idle})
	_, _ = m.Update(sessionMessagesMsg{})
	// A failed fetch still must unblock settlement — otherwise a transient
	// error strands the task until its outer timeout aborts it.
	_, cmd := m.Update(pendingPermissionErrMsg{err: errors.New("fetch failed")})
	assertQuitCommand(t, cmd)
	if result := m.Result(); result.Status != agent.StatusIdle || result.Err != nil {
		t.Fatalf("Result = %+v", result)
	}
}

func TestSessionTaskTimeoutAbortsBeforeExit(t *testing.T) {
	t.Parallel()

	m := newTestSessionTask(t)
	_, cmd := m.Update(sessionTaskTimeoutMsg{})
	if cmd == nil {
		t.Fatal("timeout produced no abort command")
	}
	if !errors.Is(m.Result().Err, ErrSessionTaskTimeout) {
		t.Fatalf("Result error = %v", m.Result().Err)
	}
	_, cmd = m.Update(sessionTaskAbortResultMsg{})
	assertQuitCommand(t, cmd)
}

func TestNewSessionTaskModelRequiresBoundedOptions(t *testing.T) {
	t.Parallel()

	tests := []TaskOptions{
		{Timeout: time.Minute, MaxVisibleLines: 8},
		{Title: "task", MaxVisibleLines: 8},
		{Title: "task", Timeout: time.Minute},
	}
	for _, opts := range tests {
		if _, err := NewSessionTaskModel(nil, "session-1", opts); err == nil {
			t.Fatalf("NewSessionTaskModel(%+v) succeeded", opts)
		}
	}
}

func newTestSessionTask(t *testing.T) *SessionTaskModel {
	t.Helper()
	m, err := NewSessionTaskModel(nil, "session-1", TaskOptions{
		Title:           "One-time preview setup",
		Timeout:         time.Minute,
		MaxVisibleLines: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan agent.Event)
	_, cancel := context.WithCancel(context.Background())
	m.SetEventChannel(ch, cancel)
	t.Cleanup(cancel)
	return m
}

func assertQuitCommand(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("command returned %T, want tea.QuitMsg", cmd())
	}
}
