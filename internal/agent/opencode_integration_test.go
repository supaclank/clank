// Integration tests for the OpenCode backend against a real opencode server.
//
// These tests require `opencode` to be installed and will start a real server.
// They talk to a real LLM, so they are slow and non-deterministic.
//
// Run with: go test -v -tags integration -run TestIntegration ./internal/agent/
//
//go:build integration

package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startRealOpenCodeServer starts `opencode serve --port=0` in the given
// project directory and returns the URL it's listening on.
func startRealOpenCodeServer(t *testing.T, projectDir string) (url string, cleanup func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "opencode", "serve", "--port=0")
	cmd.Dir = projectDir
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start opencode serve: %v", err)
	}

	// Wait for the URL to appear on stdout.
	urlCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("[opencode stdout] %s", line)
			if idx := strings.Index(line, "http://"); idx >= 0 {
				urlCh <- strings.TrimSpace(line[idx:])
				// Keep draining stdout so the process doesn't block.
				for scanner.Scan() {
					t.Logf("[opencode stdout] %s", scanner.Text())
				}
				return
			}
		}
		close(urlCh)
	}()

	select {
	case u, ok := <-urlCh:
		if !ok || u == "" {
			cancel()
			cmd.Process.Kill()
			t.Fatalf("opencode serve exited without printing URL")
		}
		url = u
	case <-time.After(30 * time.Second):
		cancel()
		cmd.Process.Kill()
		t.Fatalf("opencode serve did not start within 30s")
	}

	t.Logf("opencode server started at %s", url)

	cleanup = func() {
		cancel()
		cmd.Process.Kill()
		cmd.Wait()
	}
	return url, cleanup
}

// TestIntegrationOpenCodeBackend_SendPrompt starts a real opencode server,
// creates a backend, sends a trivial prompt, and collects all events until
// the session goes idle. This is the most basic end-to-end test.
func TestIntegrationOpenCodeBackend_SendPrompt(t *testing.T) {
	projectDir := findProjectRoot(t)
	serverURL, cleanup := startRealOpenCodeServer(t, projectDir)
	defer cleanup()

	backend := NewOpenCodeBackend(serverURL, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := StartRequest{
		Backend:    BackendOpenCode,
		ProjectDir: projectDir,
		Prompt:     "Say exactly: hello world. Nothing else.",
	}

	if err := backend.Start(ctx, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer backend.Stop()

	t.Logf("session started, sessionID=%s, status=%s", backend.SessionID(), backend.Status())

	// Collect events until idle or timeout.
	events := backend.Events()
	var collected []Event
	deadline := time.After(90 * time.Second)

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Logf("event channel closed after %d events", len(collected))
				goto done
			}
			collected = append(collected, evt)
			logEvent(t, evt)

			// Stop once we see idle.
			if evt.Type == EventStatusChange {
				if data, ok := evt.Data.(StatusChangeData); ok && data.NewStatus == StatusIdle {
					t.Logf("session went idle after %d events", len(collected))
					goto done
				}
			}

		case <-deadline:
			t.Fatalf("timed out waiting for idle after %d events", len(collected))
		}
	}

done:
	// Summarize what we got.
	t.Logf("\n=== Event Summary (%d events) ===", len(collected))
	counts := map[EventType]int{}
	for _, e := range collected {
		counts[e.Type]++
	}
	for typ, n := range counts {
		t.Logf("  %s: %d", typ, n)
	}

	// Basic assertions.
	if len(collected) == 0 {
		t.Errorf("received 0 events, expected at least status changes + text")
	}
	if counts[EventStatusChange] == 0 {
		t.Errorf("no status change events received")
	}
}

// TestIntegrationOpenCodeBackend_EventTypes sends a prompt and checks that
// we receive the expected event types with properly typed Data payloads.
func TestIntegrationOpenCodeBackend_EventTypes(t *testing.T) {
	projectDir := findProjectRoot(t)
	serverURL, cleanup := startRealOpenCodeServer(t, projectDir)
	defer cleanup()

	backend := NewOpenCodeBackend(serverURL, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := StartRequest{
		Backend:    BackendOpenCode,
		ProjectDir: projectDir,
		Prompt:     "Say exactly: test123. Nothing else.",
	}

	if err := backend.Start(ctx, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer backend.Stop()

	events := backend.Events()
	var collected []Event
	deadline := time.After(90 * time.Second)

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				goto done
			}
			collected = append(collected, evt)
			if evt.Type == EventStatusChange {
				if data, ok := evt.Data.(StatusChangeData); ok && data.NewStatus == StatusIdle {
					goto done
				}
			}
		case <-deadline:
			t.Fatalf("timed out after %d events", len(collected))
		}
	}

done:
	// Check that all Data payloads are properly typed (not map[string]interface{}).
	for i, evt := range collected {
		switch evt.Type {
		case EventStatusChange:
			if _, ok := evt.Data.(StatusChangeData); !ok {
				t.Errorf("event[%d] type=%s: Data is %T, want StatusChangeData", i, evt.Type, evt.Data)
			}
		case EventMessage:
			if _, ok := evt.Data.(MessageData); !ok {
				t.Errorf("event[%d] type=%s: Data is %T, want MessageData", i, evt.Type, evt.Data)
			}
		case EventPartUpdate:
			if _, ok := evt.Data.(PartUpdateData); !ok {
				t.Errorf("event[%d] type=%s: Data is %T, want PartUpdateData", i, evt.Type, evt.Data)
			}
		case EventPermission:
			if _, ok := evt.Data.(PermissionData); !ok {
				t.Errorf("event[%d] type=%s: Data is %T, want PermissionData", i, evt.Type, evt.Data)
			}
		case EventError:
			if _, ok := evt.Data.(ErrorData); !ok {
				t.Errorf("event[%d] type=%s: Data is %T, want ErrorData", i, evt.Type, evt.Data)
			}
		}
	}

	// Check we got at least some text content.
	var totalText string
	for _, evt := range collected {
		if evt.Type == EventPartUpdate {
			if data, ok := evt.Data.(PartUpdateData); ok && data.Part.Type == PartText {
				totalText += data.Part.Text
			}
		}
	}
	t.Logf("accumulated text: %q", totalText)
	if totalText == "" {
		t.Errorf("no text content received from agent")
	}
}

// TestIntegrationOpenCodeBackend_FollowUp sends a prompt, waits for idle,
// then sends a follow-up message. This tests the SendMessage path.
func TestIntegrationOpenCodeBackend_FollowUp(t *testing.T) {
	projectDir := findProjectRoot(t)
	serverURL, cleanup := startRealOpenCodeServer(t, projectDir)
	defer cleanup()

	backend := NewOpenCodeBackend(serverURL, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	req := StartRequest{
		Backend:    BackendOpenCode,
		ProjectDir: projectDir,
		Prompt:     "Say exactly: first. Nothing else.",
	}

	if err := backend.Start(ctx, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer backend.Stop()

	events := backend.Events()

	// Wait for first idle.
	waitForIdle(t, events, 90*time.Second)
	t.Logf("first prompt completed, sending follow-up")

	// Send follow-up.
	if err := backend.SendMessage(ctx, SendMessageOpts{Text: "Now say exactly: second. Nothing else."}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Wait for second idle.
	var followUpEvents []Event
	deadline := time.After(90 * time.Second)
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				goto done
			}
			followUpEvents = append(followUpEvents, evt)
			logEvent(t, evt)
			if evt.Type == EventStatusChange {
				if data, ok := evt.Data.(StatusChangeData); ok && data.NewStatus == StatusIdle {
					goto done
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for follow-up idle")
		}
	}

done:
	if len(followUpEvents) == 0 {
		t.Errorf("no events from follow-up message")
	}
	t.Logf("follow-up completed with %d events", len(followUpEvents))
}

// --- Helpers ---

func findProjectRoot(t *testing.T) string {
	t.Helper()
	// We're in internal/agent, project root is two levels up.
	// But to be safe, check for go.mod.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Walk up until we find go.mod.
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

func waitForIdle(t *testing.T, events <-chan Event, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatalf("event channel closed before idle")
			}
			logEvent(t, evt)
			if evt.Type == EventStatusChange {
				if data, ok := evt.Data.(StatusChangeData); ok && data.NewStatus == StatusIdle {
					return
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for idle")
		}
	}
}

func logEvent(t *testing.T, evt Event) {
	t.Helper()
	switch data := evt.Data.(type) {
	case StatusChangeData:
		t.Logf("[event] %s: %s -> %s", evt.Type, data.OldStatus, data.NewStatus)
	case MessageData:
		t.Logf("[event] %s: role=%s content=%q parts=%d", evt.Type, data.Role, truncate(data.Content, 80), len(data.Parts))
	case PartUpdateData:
		t.Logf("[event] %s: id=%s type=%s tool=%s status=%s text=%q", evt.Type, data.Part.ID, data.Part.Type, data.Part.Tool, data.Part.Status, truncate(data.Part.Text, 80))
	case PermissionData:
		t.Logf("[event] %s: id=%s tool=%s desc=%q", evt.Type, data.RequestID, data.Tool, data.Description)
	case ErrorData:
		t.Logf("[event] %s: %s", evt.Type, data.Message)
	default:
		t.Logf("[event] %s: %T %v", evt.Type, evt.Data, evt.Data)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...(%d more)", len(s)-n)
}
