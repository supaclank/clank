// Integration tests for the Session View TUI model against a real opencode server.
//
// These tests verify that real events from the daemon produce the correct
// display entries in the TUI model.
//
// Run with: go test -v -tags integration -run TestIntegration -timeout 180s ./internal/tui/
//
//go:build integration

package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
)

func startTestDaemonForTUI(t *testing.T) (*daemon.Client, func()) {
	t.Helper()

	sockDir, err := os.MkdirTemp("/tmp", "clank-tui-integ-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	sockPath := sockDir + "/test.sock"
	pidPath := sockDir + "/test.pid"

	d := daemon.NewWithPaths(sockPath, pidPath)
	factory := daemon.NewDefaultBackendFactory()
	d.BackendFactory = factory.Create
	d.OnShutdown = factory.StopAll

	started := make(chan struct{})
	go func() {
		close(started)
		d.Run()
	}()
	<-started
	time.Sleep(200 * time.Millisecond)

	client := daemon.NewClient(sockPath)

	cleanup := func() {
		d.Stop()
		time.Sleep(500 * time.Millisecond)
		os.RemoveAll(sockDir)
	}
	return client, cleanup
}

// TestIntegrationSessionView_EntriesFromRealEvents creates a real session,
// feeds daemon SSE events into the SessionViewModel's handleEvent, and
// checks that the correct display entries are produced.
func TestIntegrationSessionView_EntriesFromRealEvents(t *testing.T) {
	client, cleanup := startTestDaemonForTUI(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Subscribe to events.
	events, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	// Create session.
	projectDir := findProjectRoot(t)
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: projectDir,
		Prompt:     "Say exactly: tui-test. Nothing else.",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Create the TUI model (same as codeCmd does).
	model := NewSessionViewModel(client, info.ID)
	model.info = info // simulate sessionInfoMsg
	model.entries = append(model.entries, displayEntry{
		kind:    entryUser,
		content: info.Prompt,
	})

	// Feed events into the model until idle.
	deadline := time.After(90 * time.Second)
	eventCount := 0
	gotIdle := false

	for !gotIdle {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatalf("event channel closed")
			}
			if evt.SessionID != info.ID && evt.Type != agent.EventSessionCreate {
				continue
			}
			eventCount++
			t.Logf("[event %d] type=%s data=%T", eventCount, evt.Type, evt.Data)

			// Log details.
			switch data := evt.Data.(type) {
			case agent.StatusChangeData:
				t.Logf("  status: %s -> %s", data.OldStatus, data.NewStatus)
			case agent.PartUpdateData:
				t.Logf("  part: id=%s type=%s text=%q", data.Part.ID, data.Part.Type, data.Part.Text)
			case agent.MessageData:
				t.Logf("  message: role=%s content=%q parts=%d", data.Role, data.Content, len(data.Parts))
			}

			// Feed into TUI model.
			model.handleEvent(evt)

			// Check for idle.
			if evt.Type == agent.EventStatusChange {
				if d, ok := evt.Data.(agent.StatusChangeData); ok && d.NewStatus == agent.StatusIdle {
					gotIdle = true
				}
			}

		case <-deadline:
			t.Fatalf("timed out after %d events", eventCount)
		}
	}

	// Now inspect the TUI entries.
	t.Logf("\n=== Display Entries (%d) ===", len(model.entries))
	for i, e := range model.entries {
		t.Logf("  [%d] kind=%d partID=%q content=%q", i, e.kind, e.partID, e.content)
	}

	// Assertions.
	if len(model.entries) == 0 {
		t.Fatalf("no entries produced")
	}

	// Should have the initial user prompt.
	if model.entries[0].kind != entryUser {
		t.Errorf("entries[0] kind=%d, want entryUser(%d)", model.entries[0].kind, entryUser)
	}

	// Should have at least one agent text entry.
	var agentText string
	for _, e := range model.entries {
		if e.kind == entryText {
			agentText += e.content
		}
	}
	t.Logf("total agent text: %q", agentText)
	if agentText == "" {
		t.Errorf("no agent text entries found — this is the bug!")
	}

	// Should have status entries.
	var statusEntries []string
	for _, e := range model.entries {
		if e.kind == entryStatus {
			statusEntries = append(statusEntries, e.content)
		}
	}
	t.Logf("status entries: %v", statusEntries)
	if len(statusEntries) < 2 {
		t.Errorf("expected at least 2 status entries, got %d", len(statusEntries))
	}
}

func findProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			t.Fatalf("could not find project root from %s", dir)
		}
		dir = parent
	}
}
