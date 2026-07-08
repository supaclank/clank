package clankcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/hosttest"
)

func pickerSession(id, localPath string, updatedAgo time.Duration, vis agent.SessionVisibility) agent.SessionInfo {
	return agent.SessionInfo{
		ID:         id,
		GitRef:     agent.GitRef{LocalPath: localPath},
		UpdatedAt:  time.Now().Add(-updatedAgo),
		Visibility: vis,
	}
}

func TestRankSessionsForPick_Order(t *testing.T) {
	t.Parallel()

	sessions := []agent.SessionInfo{
		pickerSession("other-new", "/elsewhere", 1*time.Minute, ""),
		pickerSession("proj-old", "/proj", 3*time.Hour, ""),
		pickerSession("proj-new", "/proj", 10*time.Minute, ""),
		pickerSession("last", "/proj", 24*time.Hour, ""),
		pickerSession("archived", "/proj", 1*time.Minute, agent.VisibilityArchived),
	}

	got := rankSessionsForPick(sessions, "/proj", "last")

	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	want := "last proj-new proj-old other-new"
	if strings.Join(ids, " ") != want {
		t.Errorf("ranking:\n got %v\nwant %s", ids, want)
	}
}

func TestRankSessionsForPick_CapsList(t *testing.T) {
	t.Parallel()

	var sessions []agent.SessionInfo
	for i := 0; i < maxPickerSessions+5; i++ {
		sessions = append(sessions, pickerSession(strings.Repeat("x", i+1), "/p", time.Duration(i)*time.Minute, ""))
	}
	if got := len(rankSessionsForPick(sessions, "/p", "")); got != maxPickerSessions {
		t.Errorf("capped length: got %d, want %d", got, maxPickerSessions)
	}
}

func keyPress(k string) tea.Msg {
	switch k {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		r := []rune(k)[0]
		return tea.KeyPressMsg{Code: r, Text: k}
	}
}

func TestSessionPickModel_SelectsWithCursor(t *testing.T) {
	t.Parallel()

	m := newSessionPickModel([]agent.SessionInfo{
		pickerSession("first", "/p", time.Minute, ""),
		pickerSession("second", "/p", time.Hour, ""),
	})

	next, _ := m.Update(keyPress("down"))
	next, cmd := next.(sessionPickModel).Update(keyPress("enter"))
	got := next.(sessionPickModel)

	if got.chosenID != "second" {
		t.Errorf("chosenID: got %q, want second", got.chosenID)
	}
	if got.canceled {
		t.Error("canceled should be false on enter")
	}
	if cmd == nil {
		t.Error("enter should quit the program")
	}
}

func TestSessionPickModel_EscCancels(t *testing.T) {
	t.Parallel()

	m := newSessionPickModel([]agent.SessionInfo{pickerSession("only", "/p", time.Minute, "")})
	next, cmd := m.Update(keyPress("esc"))
	got := next.(sessionPickModel)

	if !got.canceled || got.chosenID != "" {
		t.Errorf("esc: canceled=%v chosenID=%q, want canceled and empty", got.canceled, got.chosenID)
	}
	if cmd == nil {
		t.Error("esc should quit the program")
	}
}

// TestResolveTargetSession_PickerNeedsTTY: --to in a non-interactive
// context (pipes, scripts, tests) must fail fast pointing at --session,
// not hang on an invisible picker. Stdin is forced to a pipe rather
// than relying on the test process's own stdin, which is a real TTY
// (and would reach the nil client) when tests run in an interactive
// terminal instead of CI.
func TestResolveTargetSession_PickerNeedsTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig }() // not parallel-safe: mutates process-global stdin

	_, err = resolveTargetSession(context.Background(), nil, "/p", true, "")
	if err == nil {
		t.Fatal("expected a no-TTY error for --to, got nil")
	}
	if !strings.Contains(err.Error(), "--session") {
		t.Errorf("error %q should point the user at --session", err)
	}
}

// TestDropPickCanceled: a cancelled picker exits clean; any other error
// passes through unchanged.
func TestDropPickCanceled(t *testing.T) {
	t.Parallel()

	if err := dropPickCanceled(errPickCanceled); err != nil {
		t.Errorf("errPickCanceled should be dropped, got %v", err)
	}
	other := errors.New("boom")
	if err := dropPickCanceled(other); err != other {
		t.Errorf("other errors should pass through unchanged, got %v", err)
	}
}

// TestResolveTargetSession_FlagBypassesPicker: an explicit --session id
// resolves without any terminal or daemon interaction.
func TestResolveTargetSession_FlagBypassesPicker(t *testing.T) {
	t.Parallel()

	got, err := resolveTargetSession(context.Background(), nil, "/p", false, "ses-123")
	if err != nil || got != "ses-123" {
		t.Errorf("got (%q, %v), want (ses-123, nil)", got, err)
	}

	got, err = resolveTargetSession(context.Background(), nil, "/p", false, "")
	if err != nil || got != "" {
		t.Errorf("no flags: got (%q, %v), want empty target for a new session", got, err)
	}
}

// TestRunPrompt_SendsToExistingSession pins the --session/--to delivery
// path: the prompt reaches the existing session as a follow-up message
// (backend Send, not OpenAndSend), and the target becomes the cwd's
// last session.
func TestRunPrompt_SendsToExistingSession(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed an existing session the follow-up will target.
	info, err := client.Sessions().Create(ctx, newStartRequest(agent.BackendOpenCode, repo, "", "", "original task"))
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	var out, errOut bytes.Buffer
	err = runPrompt(ctx, client, &out, &errOut, promptOpts{
		backend:         agent.BackendOpenCode,
		projectDir:      repo,
		prompt:          "also check the logs",
		targetSessionID: info.ID,
	})
	if err != nil {
		t.Fatalf("runPrompt: %v", err)
	}

	if got := stub.Last().LastSendOpts().Text; got != "also check the logs" {
		t.Errorf("backend received %q, want the follow-up text", got)
	}
	if !strings.Contains(out.String(), "Sent to session "+info.ID) {
		t.Errorf("output %q lacks 'Sent to session %s'", out.String(), info.ID)
	}
}

// TestRunPrompt_UnknownTargetSessionFails: a bad --session id must
// surface the daemon's not-found error, not silently create a session.
func TestRunPrompt_UnknownTargetSessionFails(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	client, stub := newTestHost(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runPrompt(ctx, client, &bytes.Buffer{}, &bytes.Buffer{}, promptOpts{
		backend:         agent.BackendOpenCode,
		projectDir:      t.TempDir(),
		prompt:          "hello",
		targetSessionID: "does-not-exist",
	})
	if err == nil {
		t.Fatal("expected an error for an unknown target session, got nil")
	}
	if stub.Last() != nil {
		t.Error("no backend should have been created for a failed targeted send")
	}
}
