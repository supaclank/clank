// Integration tests for the full daemon -> opencode backend -> real server path.
//
// These tests exercise the exact same path the TUI uses:
// TUI -> daemon client -> daemon HTTP -> opencode backend -> real opencode server
//
// Run with: go test -v -tags integration -run TestIntegration -timeout 180s ./internal/daemon/
//
//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// startTestDaemonWithRealOpenCode creates a daemon that uses
// DefaultBackendFactory (real opencode server management).
func startTestDaemonWithRealOpenCode(t *testing.T) (*Client, func()) {
	t.Helper()

	sockDir, err := os.MkdirTemp("/tmp", "clank-integ-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	sockPath := sockDir + "/test.sock"
	pidPath := sockDir + "/test.pid"

	d := NewWithPaths(sockPath, pidPath)
	factory := NewDefaultBackendFactory()
	d.BackendFactory = factory.Create
	d.OnShutdown = factory.StopAll

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		d.Run()
	}()
	<-started
	// Wait for socket to be ready.
	time.Sleep(200 * time.Millisecond)

	client := NewClient(sockPath)

	cleanup := func() {
		cancel()
		_ = ctx // keep ctx referenced
		d.Stop()
		time.Sleep(500 * time.Millisecond)
		os.RemoveAll(sockDir)
	}
	return client, cleanup
}

// TestIntegrationDaemon_CreateSessionAndStreamEvents tests the full path:
// create session via daemon API, subscribe to SSE, collect events until idle.
func TestIntegrationDaemon_CreateSessionAndStreamEvents(t *testing.T) {
	client, cleanup := startTestDaemonWithRealOpenCode(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Subscribe to events BEFORE creating the session.
	events, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	projectDir := findProjectRoot(t)
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: projectDir,
		Prompt:     "Say exactly: daemon-test. Nothing else.",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Logf("session created: id=%s status=%s", info.ID, info.Status)

	// Collect all events for our session until idle.
	var collected []agent.Event
	deadline := time.After(90 * time.Second)

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Logf("event channel closed after %d events", len(collected))
				goto done
			}
			// Only log events for our session.
			if evt.SessionID == info.ID || evt.Type == agent.EventSessionCreate {
				collected = append(collected, evt)
				logEvent(t, evt)
			}

			if evt.SessionID == info.ID && evt.Type == agent.EventStatusChange {
				if data, ok := evt.Data.(agent.StatusChangeData); ok && data.NewStatus == agent.StatusIdle {
					t.Logf("session went idle after %d events", len(collected))
					goto done
				}
			}

		case <-deadline:
			t.Fatalf("timed out after %d events", len(collected))
		}
	}

done:
	// Verify events are properly typed (the round-trip bug).
	for i, evt := range collected {
		switch evt.Type {
		case agent.EventStatusChange:
			if _, ok := evt.Data.(agent.StatusChangeData); !ok {
				t.Errorf("event[%d] %s: Data is %T, want StatusChangeData", i, evt.Type, evt.Data)
			}
		case agent.EventMessage:
			if _, ok := evt.Data.(agent.MessageData); !ok {
				t.Errorf("event[%d] %s: Data is %T, want MessageData", i, evt.Type, evt.Data)
			}
		case agent.EventPartUpdate:
			if _, ok := evt.Data.(agent.PartUpdateData); !ok {
				t.Errorf("event[%d] %s: Data is %T, want PartUpdateData", i, evt.Type, evt.Data)
			}
		case agent.EventError:
			if _, ok := evt.Data.(agent.ErrorData); !ok {
				t.Errorf("event[%d] %s: Data is %T, want ErrorData", i, evt.Type, evt.Data)
			}
		}
	}

	// Verify we got text content.
	var totalText string
	for _, evt := range collected {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok && data.Part.Type == agent.PartText {
				totalText += data.Part.Text
			}
		}
	}
	t.Logf("accumulated text: %q", totalText)
	if totalText == "" {
		t.Errorf("no text content received through daemon")
	}

	// Verify session status via API.
	refreshed, err := client.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	t.Logf("final session status: %s", refreshed.Status)
	if refreshed.Status != agent.StatusIdle {
		t.Errorf("session status = %s, want idle", refreshed.Status)
	}

	// Summary.
	t.Logf("\n=== Event Summary (%d events) ===", len(collected))
	counts := map[agent.EventType]int{}
	for _, e := range collected {
		counts[e.Type]++
	}
	for typ, n := range counts {
		t.Logf("  %s: %d", typ, n)
	}
}

// --- Helpers ---

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

func logEvent(t *testing.T, evt agent.Event) {
	t.Helper()
	switch data := evt.Data.(type) {
	case agent.StatusChangeData:
		t.Logf("[event] session=%s %s: %s -> %s", short(evt.SessionID), evt.Type, data.OldStatus, data.NewStatus)
	case agent.MessageData:
		t.Logf("[event] session=%s %s: role=%s content=%q parts=%d", short(evt.SessionID), evt.Type, data.Role, truncate(data.Content, 60), len(data.Parts))
	case agent.PartUpdateData:
		t.Logf("[event] session=%s %s: id=%s type=%s tool=%s status=%s text=%q",
			short(evt.SessionID), evt.Type, short(data.Part.ID), data.Part.Type, data.Part.Tool, data.Part.Status, truncate(data.Part.Text, 60))
	case agent.PermissionData:
		t.Logf("[event] session=%s %s: tool=%s desc=%q", short(evt.SessionID), evt.Type, data.Tool, data.Description)
	case agent.ErrorData:
		t.Logf("[event] session=%s %s: %s", short(evt.SessionID), evt.Type, data.Message)
	default:
		t.Logf("[event] session=%s %s: %T", short(evt.SessionID), evt.Type, evt.Data)
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...(%d more)", len(s)-n)
}
