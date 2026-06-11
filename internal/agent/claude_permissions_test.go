package agent_test

import (
	"context"
	"testing"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"

	"github.com/acksell/clank/internal/agent"
)

// waitForEventType drains the events channel until an event of the target type
// is observed, returning it.
func waitForEventType(t *testing.T, ch <-chan agent.Event, target agent.EventType, timeout time.Duration) agent.Event {
	t.Helper()
	timer := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatalf("events channel closed before event %s", target)
			}
			if evt.Type == target {
				return evt
			}
		case <-timer:
			t.Fatalf("timed out waiting for event %s", target)
		}
	}
}

// captureOpenOptions opens the backend through a capturing ClientFactory and
// returns the resolved spawn options the SDK would have received.
func captureOpenOptions(t *testing.T, b *agent.ClaudeCodeBackend, transport *mockTransport) claudecode.Options {
	t.Helper()
	var captured []claudecode.Option
	b.ClientFactory = func(opts ...claudecode.Option) claudecode.Client {
		captured = opts
		return claudecode.NewClientWithTransport(transport, opts...)
	}
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	var resolved claudecode.Options
	for _, opt := range captured {
		opt(&resolved)
	}
	return resolved
}

// A user-picked permission mode must propagate through OpenAndSend → Open as
// claudecode.WithPermissionMode on the spawn options.
func TestClaudeCodeBackend_PermissionMode_PropagatesToSpawnOptions(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())

	var captured []claudecode.Option
	b.ClientFactory = func(opts ...claudecode.Option) claudecode.Client {
		captured = opts
		return claudecode.NewClientWithTransport(transport, opts...)
	}

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{
		Text:           "hi",
		PermissionMode: agent.ClaudePermPlan,
	}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	defer b.Stop()

	var resolved claudecode.Options
	for _, opt := range captured {
		opt(&resolved)
	}
	if resolved.PermissionMode == nil || *resolved.PermissionMode != claudecode.PermissionModePlan {
		got := "<nil>"
		if resolved.PermissionMode != nil {
			got = string(*resolved.PermissionMode)
		}
		t.Errorf("Options.PermissionMode=%q, want plan", got)
	}
}

// A session with no explicit mode must launch in bypassPermissions — the
// product default that makes edits work out of the box.
func TestClaudeCodeBackend_PermissionMode_DefaultsToBypass(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)
	if resolved.PermissionMode == nil || *resolved.PermissionMode != claudecode.PermissionModeBypassPermissions {
		got := "<nil>"
		if resolved.PermissionMode != nil {
			got = string(*resolved.PermissionMode)
		}
		t.Errorf("default Options.PermissionMode=%q, want bypassPermissions", got)
	}
}

// CanUseTool must be wired so the SDK routes permission prompts to clank
// instead of auto-denying them.
func TestClaudeCodeBackend_CanUseTool_Wired(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)
	if resolved.CanUseTool == nil {
		t.Fatal("CanUseTool callback not wired; the SDK would auto-deny all tools")
	}
}

// Changing the mode on a follow-up must issue exactly one SetPermissionMode
// control call, and resending the same mode must not issue another.
func TestClaudeCodeBackend_PermissionMode_RuntimeChange(t *testing.T) {
	t.Parallel()
	sessionID := "claude-perm-runtime"
	result := "done"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID, Result: &result},
	})

	b := newTestBackend(t, transport)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{
		Text:           "first",
		PermissionMode: agent.ClaudePermBypass,
	}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Launch value goes through WithPermissionMode, not SetPermissionMode.
	if got := transport.recordedPermissionModes(); len(got) != 0 {
		t.Fatalf("SetPermissionMode called during launch: %v", got)
	}

	// Switch to plan on a follow-up → one SetPermissionMode("plan").
	if err := b.Send(context.Background(), agent.SendMessageOpts{
		Text:           "second",
		PermissionMode: agent.ClaudePermPlan,
	}); err != nil {
		t.Fatalf("Send (change): %v", err)
	}
	if got := transport.recordedPermissionModes(); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("recordedPermissionModes=%v, want [plan]", got)
	}

	// Resending the same mode must be a no-op.
	if err := b.Send(context.Background(), agent.SendMessageOpts{
		Text:           "third",
		PermissionMode: agent.ClaudePermPlan,
	}); err != nil {
		t.Fatalf("Send (same): %v", err)
	}
	if got := transport.recordedPermissionModes(); len(got) != 1 {
		t.Fatalf("recordedPermissionModes=%v, want still [plan] (no duplicate call)", got)
	}
}

// A permission request must surface as an EventPermission and block until the
// user's decision arrives via RespondPermission. allow=true → Allow.
func TestClaudeCodeBackend_Permission_Allow(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)

	type cbResult struct {
		res any
		err error
	}
	input := map[string]any{"command": "ls -la"}
	done := make(chan cbResult, 1)
	go func() {
		res, err := resolved.CanUseTool(context.Background(), "Bash", input, nil)
		done <- cbResult{res, err}
	}()

	evt := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)
	data, ok := evt.Data.(agent.PermissionData)
	if !ok {
		t.Fatalf("EventPermission data type=%T, want PermissionData", evt.Data)
	}
	if data.RequestID == "" {
		t.Error("PermissionData.RequestID is empty")
	}
	if data.Tool != "Bash" {
		t.Errorf("PermissionData.Tool=%q, want Bash", data.Tool)
	}
	if data.Description != "Bash: ls -la" {
		t.Errorf("PermissionData.Description=%q, want %q", data.Description, "Bash: ls -la")
	}

	if err := b.RespondPermission(context.Background(), data.RequestID, true, ""); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("callback error: %v", r.err)
		}
		allow, ok := r.res.(claudecode.PermissionResultAllow)
		if !ok {
			t.Fatalf("callback result=%T, want PermissionResultAllow", r.res)
		}
		// The CLI's permission schema requires updatedInput on the allow branch;
		// a bare allow fails every tool with a ZodError. The backend must echo
		// the tool input back as updatedInput.
		if allow.UpdatedInput == nil {
			t.Fatal("PermissionResultAllow.UpdatedInput is nil; CLI rejects bare allow with a ZodError")
		}
		if got := allow.UpdatedInput["command"]; got != "ls -la" {
			t.Errorf("UpdatedInput[command]=%v, want %q (input must be echoed unchanged)", got, "ls -la")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not return after RespondPermission(allow)")
	}
}

// allow=false → Deny, and the deny message is forwarded to the model verbatim
// (this is how plan-review comments reach Claude so it can revise).
func TestClaudeCodeBackend_Permission_Deny(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)

	done := make(chan any, 1)
	go func() {
		res, _ := resolved.CanUseTool(context.Background(), "Write", map[string]any{"file_path": "/tmp/x"}, nil)
		done <- res
	}()

	const denyMsg = "Please revise: tighten the Overview section."
	evt := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)
	data := evt.Data.(agent.PermissionData)
	if err := b.RespondPermission(context.Background(), data.RequestID, false, denyMsg); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}

	select {
	case res := <-done:
		deny, ok := res.(claudecode.PermissionResultDeny)
		if !ok {
			t.Fatalf("callback result=%T, want PermissionResultDeny", res)
		}
		if deny.Message != denyMsg {
			t.Errorf("deny message=%q, want %q (review comments must reach the model)", deny.Message, denyMsg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not return after RespondPermission(deny)")
	}
}

// Stop must unblock a parked permission callback (denying it) so the SDK read
// goroutine never leaks.
func TestClaudeCodeBackend_Permission_CleanupOnStop(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())

	resolved := captureOpenOptions(t, b, transport)

	done := make(chan any, 1)
	go func() {
		res, _ := resolved.CanUseTool(context.Background(), "Bash", map[string]any{"command": "rm -rf /"}, nil)
		done <- res
	}()

	waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)

	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case res := <-done:
		if _, ok := res.(claudecode.PermissionResultDeny); !ok {
			t.Errorf("callback result=%T, want PermissionResultDeny after Stop", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not unblock the parked permission callback")
	}
}

// Abort must unblock a parked permission callback (denying it) so the read
// goroutine is freed immediately rather than waiting for the interrupt.
func TestClaudeCodeBackend_Permission_AbortUnblocks(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)

	done := make(chan any, 1)
	go func() {
		res, _ := resolved.CanUseTool(context.Background(), "Bash", map[string]any{"command": "curl evil"}, nil)
		done <- res
	}()

	waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)

	if err := b.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	select {
	case res := <-done:
		if _, ok := res.(claudecode.PermissionResultDeny); !ok {
			t.Errorf("callback result=%T, want PermissionResultDeny after Abort", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Abort did not unblock the parked permission callback")
	}
}

// Send must use the backend context (b.ctx) — not the caller context — when
// calling SetPermissionMode, so a cancelled request cannot corrupt backend state
// by aborting the mode-change mid-flight.
func TestClaudeCodeBackend_PermissionMode_RuntimeChange_UsesBackendCtx(t *testing.T) {
	t.Parallel()
	sessionID := "claude-perm-ctx"
	result := "done"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID, Result: &result},
	})

	b := newTestBackend(t, transport)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{
		Text:           "first",
		PermissionMode: agent.ClaudePermBypass,
	}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Cancel the caller ctx before requesting a mode change.
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// SetPermissionMode must succeed because it uses b.ctx (not the cancelled callerCtx).
	transport.onSetPermMode = func(ctx context.Context, _ string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}

	if err := b.Send(callerCtx, agent.SendMessageOpts{
		Text:           "second",
		PermissionMode: agent.ClaudePermPlan,
	}); err != nil {
		t.Fatalf("Send with cancelled caller ctx: %v (b.ctx must be used, not caller ctx)", err)
	}
}

// A Send issued while a permission prompt is still pending must fast-fail
// instead of deadlocking. The parked handleCanUseTool blocks the SDK read
// goroutine, so a mode-changing Send (what the mobile plan-Approve does) would
// otherwise wait the full SetPermissionMode control timeout and then flip the
// session to StatusError, bricking it.
func TestClaudeCodeBackend_Send_BlockedWhilePermissionPending(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)

	// Park a permission, mirroring a pending ExitPlanMode prompt.
	go func() {
		_, _ = resolved.CanUseTool(context.Background(), "ExitPlanMode", map[string]any{}, nil)
	}()
	waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)

	errCh := make(chan error, 1)
	go func() {
		errCh <- b.Send(context.Background(), agent.SendMessageOpts{
			Text:           "approved",
			PermissionMode: agent.ClaudePermBypass,
		})
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Send while a permission was pending returned nil; want a fast-fail error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked while a permission was pending (deadlock); want an immediate error")
	}

	// The guard must run before the mode change, so nothing leaks to the CLI.
	if got := transport.recordedPermissionModes(); len(got) != 0 {
		t.Errorf("SetPermissionMode called while a permission was pending: %v", got)
	}
	// The session must stay usable, not be flipped to error.
	if b.Status() == agent.StatusError {
		t.Error("Send guard flipped the session to StatusError; want it to remain usable")
	}
}

// A permission prompt must carry the tool_use id of the block it gates (derived
// from the stream) so a client can correlate the prompt with the tool-call card
// it already rendered instead of guessing by tool name.
func TestClaudeCodeBackend_Permission_CarriesToolUseID(t *testing.T) {
	t.Parallel()
	const sessionID = "claude-tooluse"
	const toolUseID = "toolu_plan_abc"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg_1"},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   toolUseID,
					"name": "ExitPlanMode",
				},
			},
		},
	})
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()
	resolved := captureOpenOptions(t, b, transport)

	// Wait for the tool_use part so handleContentBlockStart has recorded the id
	// before the permission (which reads it) is raised.
	waitForToolPart(t, b.Events(), "ExitPlanMode", 2*time.Second)

	go func() {
		_, _ = resolved.CanUseTool(context.Background(), "ExitPlanMode", map[string]any{}, nil)
	}()
	evt := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)
	data := evt.Data.(agent.PermissionData)
	if data.ToolUseID != toolUseID {
		t.Errorf("PermissionData.ToolUseID=%q, want %q", data.ToolUseID, toolUseID)
	}
}

// waitForToolPart drains events until a tool-call part for the given tool is seen.
func waitForToolPart(t *testing.T, ch <-chan agent.Event, tool string, timeout time.Duration) {
	t.Helper()
	timer := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatalf("events channel closed before tool part %s", tool)
			}
			if evt.Type == agent.EventPartUpdate {
				if d, ok := evt.Data.(agent.PartUpdateData); ok && d.Part.Tool == tool {
					return
				}
			}
		case <-timer:
			t.Fatalf("timed out waiting for tool part %s", tool)
		}
	}
}

// RespondPermission for an unknown ID must fail fast rather than silently
// succeed or panic.
func TestClaudeCodeBackend_RespondPermission_UnknownID(t *testing.T) {
	t.Parallel()
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	if err := b.RespondPermission(context.Background(), "does-not-exist", true, ""); err == nil {
		t.Error("RespondPermission(unknown) = nil, want error")
	}
}
