package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCodexBackendEndToEnd drives a real `codex app-server` through the full
// backend lifecycle: open (thread/start with developerInstructions), send,
// stream events to idle, transcript read, stop.
//
// Requires a codex binary and authenticated CODEX_HOME, so it is opt-in:
//
//	CLANK_CODEX_E2E_BIN=/path/to/codex CODEX_HOME=/path/to/home go test -run TestCodexBackendEndToEnd
func TestCodexBackendEndToEnd(t *testing.T) {
	bin := os.Getenv("CLANK_CODEX_E2E_BIN")
	if bin == "" {
		t.Skip("set CLANK_CODEX_E2E_BIN (and CODEX_HOME) to run the codex e2e test")
	}

	workDir := t.TempDir()
	if out, err := exec.Command("git", "-C", workDir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	b := NewCodexBackend(workDir)
	b.CodexPath = bin
	b.SystemPrompt = "Begin the very first sentence of every reply with the word BANANA."
	defer b.Stop()

	if err := b.OpenAndSend(ctx, SendMessageOpts{
		Text:           "Reply with exactly: OK",
		PermissionMode: ClaudePermBypass,
	}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	if b.SessionID() == "" {
		t.Fatal("no thread id after open")
	}

	var sawAssistantShell, sawDelta, sawIdle bool
	var externalID string
	deadline := time.After(2 * time.Minute)
events:
	for {
		select {
		case ev, ok := <-b.Events():
			if !ok {
				t.Fatal("event channel closed before idle")
			}
			if ev.ExternalID != "" {
				externalID = ev.ExternalID
			}
			switch d := ev.Data.(type) {
			case MessageData:
				if d.Role == "assistant" {
					sawAssistantShell = true
				}
			case PartUpdateData:
				if d.IsDelta && d.Part.Type == PartText {
					sawDelta = true
				}
			case StatusChangeData:
				if d.NewStatus == StatusIdle && sawAssistantShell {
					sawIdle = true
					break events
				}
				if d.NewStatus == StatusError || d.NewStatus == StatusDead {
					t.Fatalf("session entered %s", d.NewStatus)
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for idle (shell=%t delta=%t)", sawAssistantShell, sawDelta)
		}
	}

	if !sawIdle || !sawDelta {
		t.Fatalf("incomplete stream: shell=%t delta=%t idle=%t", sawAssistantShell, sawDelta, sawIdle)
	}
	if externalID != b.SessionID() {
		t.Errorf("event ExternalID %q != SessionID %q", externalID, b.SessionID())
	}

	msgs, err := b.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var reply string
	for _, m := range msgs {
		if m.Role == "assistant" {
			reply = m.Content
		}
	}
	if !strings.HasPrefix(reply, "BANANA") {
		t.Errorf("developerInstructions not applied; assistant reply = %q", reply)
	}
	if !strings.Contains(reply, "OK") {
		t.Errorf("assistant reply = %q, want it to contain OK", reply)
	}
}
