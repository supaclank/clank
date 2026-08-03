package acp_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	acpx "github.com/supaclank/clank/internal/agent/acp"
	"github.com/supaclank/clank/internal/agent/acp/acptest"
	sdk "github.com/coder/acp-go-sdk"
)

// backendFixture wires a Backend directly to a ScriptedAgent (no
// supervisor) and records every emitted event.
type backendFixture struct {
	t       *testing.T
	agent   *acptest.ScriptedAgent
	backend *acpx.Backend

	mu     sync.Mutex
	events []agent.Event
	notify chan struct{}
}

func newBackendFixture(t *testing.T, scripted *acptest.ScriptedAgent, resume string) *backendFixture {
	t.Helper()
	f := &backendFixture{t: t, agent: scripted, notify: make(chan struct{}, 256)}
	profile := testProfile(acpx.ScopeHost, nil)
	proc, err := acptest.Proc(context.Background(), profile, scripted, testLogf(t))
	if err != nil {
		t.Fatalf("acptest.Proc: %v", err)
	}
	t.Cleanup(proc.Stop)
	resolver := func(context.Context) (*acpx.AdapterConn, error) { return proc.Conn, nil }
	f.backend = acpx.NewBackend(profile, "/work", resume, "", resolver, testLogf(t))
	t.Cleanup(func() { _ = f.backend.Stop() })
	go func() {
		for e := range f.backend.Events() {
			f.mu.Lock()
			f.events = append(f.events, e)
			f.mu.Unlock()
			select {
			case f.notify <- struct{}{}:
			default:
			}
		}
	}()
	return f
}

func (f *backendFixture) snapshot() []agent.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Event(nil), f.events...)
}

// waitFor blocks until pred(events) or the timeout elapses.
func (f *backendFixture) waitFor(timeout time.Duration, pred func([]agent.Event) bool) []agent.Event {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		evts := f.snapshot()
		if pred(evts) {
			return evts
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("condition not met; events: %s", eventTypes(evts))
			return evts
		}
		select {
		case <-f.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func eventTypes(evts []agent.Event) string {
	s := ""
	for _, e := range evts {
		s += string(e.Type) + " "
	}
	return s
}

func statusOf(evts []agent.Event) agent.SessionStatus {
	last := agent.SessionStatus("")
	for _, e := range evts {
		if e.Type == agent.EventStatusChange {
			last = e.Data.(agent.StatusChangeData).NewStatus
		}
	}
	return last
}

// settledTurns counts turns that ran to a terminal state (busy → idle or
// busy → error).
//
// Waiting on "the last status is idle" is NOT a turn-completion signal:
// Open emits starting → idle before any turn exists, so that predicate
// matches the pre-turn idle and lets a test read the transcript while the
// turn is still streaming — every later assertion then races the turn it
// meant to observe. Tests wait on this instead.
func settledTurns(evts []agent.Event) int {
	settled, busy := 0, false
	for _, e := range evts {
		if e.Type != agent.EventStatusChange {
			continue
		}
		switch e.Data.(agent.StatusChangeData).NewStatus {
		case agent.StatusBusy:
			busy = true
		case agent.StatusIdle, agent.StatusError:
			if busy {
				settled++
				busy = false
			}
		}
	}
	return settled
}

// scriptEchoTurn scripts a turn: one text chunk, one tool call that
// completes, then end_turn.
func scriptEchoTurn(a *acptest.ScriptedAgent) {
	a.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		conn := a.Conn()
		push := func(u sdk.SessionUpdate) {
			_ = conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: u})
		}
		push(sdk.UpdateAgentMessageText("hello "))
		push(sdk.UpdateAgentMessageText("world"))
		push(sdk.StartToolCall("tc-1", "Terminal"))
		st := sdk.ToolCallStatusCompleted
		push(sdk.UpdateToolCall("tc-1", sdk.WithUpdateStatus(st), sdk.WithUpdateRawOutput("done")))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
}

func TestBackend_TurnLifecycle(t *testing.T) {
	t.Parallel()
	scripted := &acptest.ScriptedAgent{}
	scriptEchoTurn(scripted)
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()

	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := f.backend.SessionID(); got == "" {
		t.Fatal("SessionID empty after Open")
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	evts := f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1 && countType(evts, agent.EventMessage) >= 2
	})

	// Every event carries the external id.
	for _, e := range evts {
		if e.ExternalID == "" {
			t.Errorf("event %s missing ExternalID", e.Type)
		}
	}
	// Live message events: the full user message, then an assistant
	// SHELL. Regression for the triple-render bug: a live assistant
	// message event carrying Content or Parts makes clients render the
	// turn once per channel (streamed part + Content append + parts
	// re-add) on fresh sessions — rendering must flow through
	// EventPartUpdate only.
	var roles []string
	for _, e := range evts {
		if e.Type != agent.EventMessage {
			continue
		}
		md := e.Data.(agent.MessageData)
		roles = append(roles, md.Role)
		if md.Role == "assistant" {
			if md.Content != "" || len(md.Parts) != 0 {
				t.Errorf("assistant message event must be a shell; got Content=%q Parts=%d", md.Content, len(md.Parts))
			}
			if md.ID == "" {
				t.Error("assistant shell missing message ID")
			}
		}
	}
	if fmt.Sprint(roles) != "[user assistant]" {
		t.Errorf("live message events = %v, want [user assistant] (no carrier event)", roles)
	}
	// Tool call + result share one part id, delivered via part updates.
	var callID, resultID string
	for _, e := range evts {
		if e.Type != agent.EventPartUpdate {
			continue
		}
		p := e.Data.(agent.PartUpdateData).Part
		switch p.Type {
		case agent.PartToolCall:
			callID = p.ID
		case agent.PartToolResult:
			resultID = p.ID
			if p.Output != "done" {
				t.Errorf("tool result output = %q, want done", p.Output)
			}
		}
	}
	if callID == "" || callID != resultID {
		t.Errorf("tool parts: call=%q result=%q, want equal non-empty", callID, resultID)
	}
	// Streaming deltas arrived before the commit.
	if countDeltas(evts) < 2 {
		t.Errorf("expected streamed text deltas, got %d", countDeltas(evts))
	}
	// Messages() carries the FULL committed transcript (user, assistant
	// with text+tool parts, tool-result carrier) — the shell events never
	// thin it out.
	msgs, err := f.backend.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 3 || msgs[1].Content != "hello world" {
		t.Fatalf("Messages = %d entries (assistant content %q), want 3 with 'hello world'", len(msgs), msgs[1].Content)
	}
	if len(msgs[1].Parts) == 0 || msgs[2].Role != "user" || len(msgs[2].Parts) != 1 {
		t.Errorf("committed transcript shape wrong: assistant parts=%d carrier=%+v", len(msgs[1].Parts), msgs[2])
	}
}

func countType(evts []agent.Event, ty agent.EventType) int {
	n := 0
	for _, e := range evts {
		if e.Type == ty {
			n++
		}
	}
	return n
}

func countDeltas(evts []agent.Event) int {
	n := 0
	for _, e := range evts {
		if e.Type == agent.EventPartUpdate && e.Data.(agent.PartUpdateData).IsDelta {
			n++
		}
	}
	return n
}

func TestBackend_QueueWhileBusy(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var inFlight, maxInFlight int
	var mu sync.Mutex
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "one"}); err != nil {
		t.Fatalf("Send one: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "two"}); err != nil {
		t.Fatalf("Send two (queued): %v", err)
	}
	if got := f.backend.Status(); got != agent.StatusBusy {
		t.Fatalf("status while queued = %s, want busy", got)
	}
	close(release)
	f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("max concurrent prompts = %d, want 1 (sequential dispatch)", maxInFlight)
	}
}

func TestBackend_PermissionRoundtrip(t *testing.T) {
	t.Parallel()
	outcome := make(chan sdk.RequestPermissionResponse, 1)
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		resp, err := scripted.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "tc-9", Title: sdk.Ptr("Run tests")},
			Options: []sdk.PermissionOption{
				{OptionId: "yes", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
				{OptionId: "no", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return sdk.PromptResponse{}, err
		}
		outcome <- resp
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	evts := f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventPermission) == 1
	})
	var perm agent.PermissionData
	for _, e := range evts {
		if e.Type == agent.EventPermission {
			perm = e.Data.(agent.PermissionData)
		}
	}
	if perm.ToolUseID != "tc-9" || perm.RequestID == "" {
		t.Fatalf("permission data = %+v", perm)
	}
	if err := f.backend.RespondPermission(ctx, perm.RequestID, true, ""); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	select {
	case resp := <-outcome:
		if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "yes" {
			t.Fatalf("agent saw outcome %+v, want selected yes", resp.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent never received the permission outcome")
	}
	if err := f.backend.RespondPermission(ctx, perm.RequestID, true, ""); err == nil {
		t.Error("second RespondPermission should fail (already resolved)")
	}
}

// TestBackend_PendingPermissionsSnapshot pins the (re)join recovery
// contract behind GET /sessions/{id}/pending-permission: while requests
// are parked, PendingPermissions returns the same PermissionData the SSE
// events carried, oldest first, and answering a request removes exactly
// that entry — so a client that joined mid-block re-renders only the
// prompts still awaiting a decision.
func TestBackend_PendingPermissionsSnapshot(t *testing.T) {
	t.Parallel()
	options := []sdk.PermissionOption{
		{OptionId: "yes", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
		{OptionId: "no", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
	}
	// secondParked gates the second request so the two park in a
	// deterministic order (the snapshot promises oldest-first).
	secondParked := make(chan struct{})
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		errs := make(chan error, 2)
		request := func(toolCallID, title string) {
			_, err := scripted.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
				SessionId: p.SessionId,
				ToolCall:  sdk.ToolCallUpdate{ToolCallId: sdk.ToolCallId(toolCallID), Title: sdk.Ptr(title)},
				Options:   options,
			})
			errs <- err
		}
		go request("tc-first", "Run tests")
		<-secondParked
		go request("tc-second", "Edit file")
		for range 2 {
			if err := <-errs; err != nil {
				return sdk.PromptResponse{}, err
			}
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(f.backend.PendingPermissions()) != 0 {
		t.Fatal("fresh backend should have no pending permissions")
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventPermission) == 1
	})
	close(secondParked)
	evts := f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventPermission) == 2
	})

	var emitted []agent.PermissionData
	for _, e := range evts {
		if e.Type == agent.EventPermission {
			emitted = append(emitted, e.Data.(agent.PermissionData))
		}
	}
	perms := f.backend.PendingPermissions()
	if len(perms) != 2 {
		t.Fatalf("pending = %+v, want the 2 parked requests", perms)
	}
	for i, want := range emitted {
		if perms[i] != want {
			t.Errorf("pending[%d] = %+v, want the emitted event data %+v", i, perms[i], want)
		}
	}
	if perms[0].ToolUseID != "tc-first" || perms[1].ToolUseID != "tc-second" {
		t.Errorf("pending order = [%s, %s], want oldest first [tc-first, tc-second]",
			perms[0].ToolUseID, perms[1].ToolUseID)
	}

	// Answering the first request must leave only the second parked.
	if err := f.backend.RespondPermission(ctx, perms[0].RequestID, true, ""); err != nil {
		t.Fatalf("RespondPermission first: %v", err)
	}
	if remaining := f.backend.PendingPermissions(); len(remaining) != 1 || remaining[0].ToolUseID != "tc-second" {
		t.Fatalf("pending after first reply = %+v, want only tc-second", remaining)
	}
	if err := f.backend.RespondPermission(ctx, perms[1].RequestID, true, ""); err != nil {
		t.Fatalf("RespondPermission second: %v", err)
	}
	if remaining := f.backend.PendingPermissions(); len(remaining) != 0 {
		t.Fatalf("pending after both replies = %+v, want empty", remaining)
	}
	f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1
	})
}

// Abort must release a parked permission promptly (the SDK cancels the
// in-flight prompt's context on session/cancel, so the agent may see
// either the swept cancelled outcome or a context error — both mean the
// park was released) and settle the session to idle with no error noise.
func TestBackend_AbortReleasesPendingPermissionAndSettlesIdle(t *testing.T) {
	t.Parallel()
	released := make(chan struct{}, 1)
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		resp, err := scripted.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "tc-1"},
			Options:   []sdk.PermissionOption{{OptionId: "yes", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce}},
		})
		if err == nil && resp.Outcome.Cancelled == nil {
			t.Errorf("aborted permission outcome = %+v, want cancelled (or ctx error)", resp.Outcome)
		}
		released <- struct{}{}
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventPermission) == 1
	})
	if err := f.backend.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("parked permission was not released on abort")
	}
	evts := f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1
	})
	if countType(evts, agent.EventError) != 0 {
		t.Errorf("abort should not produce error events; got %s", eventTypes(evts))
	}
}

// Abort() before Open() hits the conn==nil early return, which used to
// leave b.aborting stuck true forever (the reset paths are all gated on a
// live conn/session). A later genuine turn failure would then be
// misread as a cancellation and silently swallowed instead of surfacing
// as StatusError.
func TestBackend_AbortBeforeOpen_DoesNotSwallowLaterTurnError(t *testing.T) {
	t.Parallel()
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		return sdk.PromptResponse{}, fmt.Errorf("provider exploded")
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Abort(ctx); err != nil {
		t.Fatalf("Abort before Open: %v", err)
	}
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	evts := f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1 && statusOf(evts) == agent.StatusError && countType(evts, agent.EventError) >= 1
	})
	if countType(evts, agent.EventError) == 0 {
		t.Errorf("turn error after a pre-Open Abort was swallowed; got %s", eventTypes(evts))
	}
}

// Same hazard as above, but via the other unreset path: Cancel's RPC
// itself fails with no turn in flight, so neither reset branch (the
// conn==nil guard, nor the post-Cancel !hadTurn branch) ever runs.
func TestBackend_AbortCancelFails_NoActiveTurn_DoesNotSwallowLaterTurnError(t *testing.T) {
	t.Parallel()
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		return sdk.PromptResponse{}, fmt.Errorf("provider exploded")
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// session/cancel is a JSON-RPC notification: the only way Cancel()
	// itself returns an error is a local send failure, e.g. an
	// already-cancelled context.
	abortCtx, cancelAbort := context.WithCancel(ctx)
	cancelAbort()
	if err := f.backend.Abort(abortCtx); err == nil {
		t.Fatal("Abort with a cancelled context: want the Cancel send error surfaced, got nil")
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	evts := f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1 && statusOf(evts) == agent.StatusError && countType(evts, agent.EventError) >= 1
	})
	if countType(evts, agent.EventError) == 0 {
		t.Errorf("turn error after a failed no-op Abort was swallowed; got %s", eventTypes(evts))
	}
}

// Stop must release a parked permission. Two valid release paths race:
// the sweep resolves it with a cancelled outcome, and Stop's bgCancel
// kills the in-flight prompt RPC whose cancellation propagates to the
// agent's RequestPermission ctx. Either way the agent unblocks and a
// non-cancelled outcome is a bug.
func TestBackend_StopReleasesPendingPermission(t *testing.T) {
	t.Parallel()
	released := make(chan struct{}, 1)
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		resp, err := scripted.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "tc-2"},
			Options:   []sdk.PermissionOption{{OptionId: "yes", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce}},
		})
		if err == nil && resp.Outcome.Cancelled == nil {
			t.Errorf("stopped permission outcome = %+v, want cancelled (or ctx error)", resp.Outcome)
		}
		released <- struct{}{}
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventPermission) == 1
	})
	if err := f.backend.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("parked permission was not released on stop")
	}
}

func TestBackend_LoadReplayBuildsTranscriptWithoutEvents(t *testing.T) {
	t.Parallel()
	scripted := &acptest.ScriptedAgent{}
	scripted.LoadSessionFn = func(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
		conn := scripted.Conn()
		push := func(u sdk.SessionUpdate) {
			_ = conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: u})
		}
		push(sdk.UpdateUserMessageText("earlier question"))
		push(sdk.UpdateAgentMessageText("earlier answer"))
		push(sdk.StartToolCall("tc-old", "Read"))
		push(sdk.UpdateToolCall("tc-old", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted), sdk.WithUpdateRawOutput("file contents")))
		return sdk.LoadSessionResponse{}, nil
	}
	f := newBackendFixture(t, scripted, "ses-resume-1")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open (resume): %v", err)
	}

	msgs, err := f.backend.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if fmt.Sprint(roles) != "[user assistant user]" {
		t.Fatalf("replayed roles = %v, want [user assistant user]", roles)
	}
	if msgs[0].Content != "earlier question" || msgs[1].Content != "earlier answer" {
		t.Errorf("replayed contents = %q / %q", msgs[0].Content, msgs[1].Content)
	}
	// Replay must not stream events: only the open status transition.
	for _, e := range f.snapshot() {
		if e.Type == agent.EventPartUpdate || e.Type == agent.EventMessage {
			t.Errorf("replay leaked live event %s", e.Type)
		}
	}
}

// session/update notifications can stream into the reducer before a
// failing session/load RPC returns, and finishReplay used to commit that
// partial state unconditionally. A retried Open on the same Backend then
// duplicated the replayed history on top of the leftover partial commit.
func TestBackend_FailedLoadSession_RetryDoesNotDuplicateMessages(t *testing.T) {
	t.Parallel()
	attempt := 0
	scripted := &acptest.ScriptedAgent{}
	scripted.LoadSessionFn = func(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
		attempt++
		conn := scripted.Conn()
		push := func(u sdk.SessionUpdate) {
			_ = conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: u})
		}
		push(sdk.UpdateUserMessageText("earlier question"))
		push(sdk.UpdateAgentMessageText("earlier answer"))
		if attempt == 1 {
			return sdk.LoadSessionResponse{}, fmt.Errorf("adapter hiccup")
		}
		return sdk.LoadSessionResponse{}, nil
	}
	f := newBackendFixture(t, scripted, "ses-resume-retry")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err == nil {
		t.Fatal("Open (resume, first attempt): want the LoadSession error surfaced, got nil")
	}
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open (resume, retry): %v", err)
	}

	msgs, err := f.backend.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if fmt.Sprint(roles) != "[user assistant]" {
		t.Fatalf("post-retry replayed roles = %v, want [user assistant] (no duplicates from the failed attempt)", roles)
	}
	// turnSeq rewinds with the rollback: the retry mints the same
	// deterministic message IDs a first-try replay would have.
	if msgs[1].ID != "ses-resume-retry:t1" {
		t.Errorf("post-retry assistant message ID = %q, want ses-resume-retry:t1", msgs[1].ID)
	}
}

func TestBackend_LateUpdates_DrainedIntoTurnThenDroppedAfterIdle(t *testing.T) {
	t.Parallel()
	scripted := &acptest.ScriptedAgent{}
	afterIdle := make(chan sdk.SessionId, 1)
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		conn := scripted.Conn()
		_ = conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.StartToolCall("tc-late", "Slow tool")})
		// Late completion lands AFTER the response resolves but inside
		// the drain window (the claude #864 class).
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = conn.SessionUpdate(context.Background(), sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateToolCall("tc-late", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted))})
			afterIdle <- p.SessionId
		}()
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1
	})
	// The drained completion must be part of the committed transcript.
	msgs, err := f.backend.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	committed := false
	for _, md := range msgs {
		for _, p := range md.Parts {
			if p.ID == "tc-late" && p.Status == agent.PartCompleted {
				committed = true
			}
		}
	}
	if !committed {
		t.Error("late tool completion was not drained into the turn")
	}

	// A stray turn-scoped update after idle is dropped, not streamed.
	sid := <-afterIdle
	before := len(f.snapshot())
	_ = scripted.Conn().SessionUpdate(context.Background(), sdk.SessionNotification{SessionId: sid, Update: sdk.UpdateToolCall("tc-late", sdk.WithUpdateStatus(sdk.ToolCallStatusFailed))})
	time.Sleep(150 * time.Millisecond)
	for _, e := range f.snapshot()[before:] {
		if e.Type == agent.EventPartUpdate || e.Type == agent.EventMessage {
			t.Errorf("post-idle turn-scoped update leaked: %s data=%+v", e.Type, e.Data)
		}
	}

	// Session-scoped updates still pass after idle (title).
	_ = scripted.Conn().SessionUpdate(context.Background(), sdk.SessionNotification{SessionId: sid, Update: sdk.SessionUpdate{SessionInfoUpdate: &sdk.SessionSessionInfoUpdate{Title: sdk.Ptr("Spike title")}}})
	f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventTitleChange) == 1
	})
}

func TestBackend_PromptErrorTurnsErrorThenRecovers(t *testing.T) {
	t.Parallel()
	var fail bool = true
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		if fail {
			return sdk.PromptResponse{}, fmt.Errorf("provider exploded")
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "boom"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1 && statusOf(evts) == agent.StatusError && countType(evts, agent.EventError) >= 1
	})
	fail = false
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "again"}); err != nil {
		t.Fatalf("Send after error: %v", err)
	}
	f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return settledTurns(evts) >= 1
	})
}

func TestBackend_ForkAndRevertUnsupported(t *testing.T) {
	t.Parallel()
	scripted := &acptest.ScriptedAgent{} // DefaultInitialize has no fork cap
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.backend.Fork(ctx, "some-message"); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Fork without capability = %v, want ErrUnsupported", err)
	}
	if err := f.backend.Revert(ctx, "m1"); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Revert = %v, want ErrUnsupported", err)
	}
	if err := f.backend.RespondQuestion(ctx, "q1", nil, false); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("RespondQuestion = %v, want ErrUnsupported", err)
	}
}

func TestBackend_StopMarksDeadAndClosesEvents(t *testing.T) {
	t.Parallel()
	scripted := &acptest.ScriptedAgent{}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return statusOf(evts) == agent.StatusDead
	})
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "nope"}); err == nil {
		t.Error("Send after Stop should error")
	}
}

// Modes are agent-owned: the advertised list surfaces untranslated via
// Modes(), chosen ids pass through to session/set_mode raw, and ids the
// agent never advertised are skipped instead of risking an error state.
func TestBackend_AgentOwnedModes(t *testing.T) {
	t.Parallel()
	setModes := make(chan string, 4)
	scripted := &acptest.ScriptedAgent{}
	scripted.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
		desc := "Read-only sandbox"
		return sdk.NewSessionResponse{
			SessionId: "codex-thread-1",
			Modes: &sdk.SessionModeState{
				CurrentModeId: "agent",
				AvailableModes: []sdk.SessionMode{
					{Id: "read-only", Name: "Read Only", Description: &desc},
					{Id: "agent", Name: "Agent"},
					{Id: "agent-full-access", Name: "Full Access"},
				},
			},
		}, nil
	}
	scripted.SetModeFn = func(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
		setModes <- string(p.ModeId)
		return sdk.SetSessionModeResponse{}, nil
	}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	current, available := f.backend.Modes()
	if current != "agent" || len(available) != 3 {
		t.Fatalf("Modes() = %q / %d modes, want agent / 3", current, len(available))
	}
	if available[0].ID != "read-only" || available[0].Name != "Read Only" || available[0].Description != "Read-only sandbox" {
		t.Errorf("advertised mode surfaced wrong: %+v", available[0])
	}

	// Advertised id passes through raw.
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go", Config: map[string]string{agent.ConfigOptionMode: "read-only"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case got := <-setModes:
		if got != "read-only" {
			t.Fatalf("set_mode id = %q, want read-only", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent never received set_mode")
	}
	f.waitFor(10*time.Second, func(evts []agent.Event) bool { return settledTurns(evts) >= 1 })

	// Unadvertised id is skipped, not sent.
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "again", Config: map[string]string{agent.ConfigOptionMode: "bogus-mode"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventMessage) >= 2 && settledTurns(evts) >= 2
	})
	select {
	case got := <-setModes:
		t.Fatalf("unadvertised mode id was sent to the agent: %q", got)
	default:
	}
}

// Resumed sessions must surface the agent's modes and models. session/load
// returns the same modes + configOptions as session/new, and dropping them
// left every reopened session with an empty mode picker (clients then fell
// back to a hardcoded list) and an empty model picker.
func TestBackend_ResumeCapturesModesAndModels(t *testing.T) {
	t.Parallel()
	desc := "Read-only sandbox"
	state := &sdk.SessionModeState{
		CurrentModeId: "agent",
		AvailableModes: []sdk.SessionMode{
			{Id: "read-only", Name: "Read Only", Description: &desc},
			{Id: "agent", Name: "Agent"},
		},
	}
	category := sdk.SessionConfigOptionCategory("model")
	opts := []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
		Id:           "model",
		Name:         "Model",
		Category:     &category,
		CurrentValue: "gpt-5.2-codex",
		Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
			{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
			{Value: "o4-mini", Name: "o4-mini"},
		}},
	}}}

	scripted := &acptest.ScriptedAgent{}
	scripted.LoadSessionFn = func(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
		return sdk.LoadSessionResponse{Modes: state, ConfigOptions: opts}, nil
	}
	f := newBackendFixture(t, scripted, "ses-resume-modes")
	if err := f.backend.Open(context.Background()); err != nil {
		t.Fatalf("Open (resume): %v", err)
	}

	curMode, modes := f.backend.Modes()
	if curMode != "agent" || len(modes) != 2 || modes[0].ID != "read-only" || modes[0].Description != "Read-only sandbox" {
		t.Errorf("resumed modes = %q / %+v, want the agent-advertised list", curMode, modes)
	}
	curModel, models := f.backend.Models()
	if curModel != "gpt-5.2-codex" || len(models) != 2 || models[0].ID != "gpt-5.2-codex" || models[0].Name != "GPT-5.2 Codex" {
		t.Errorf("resumed models = %q / %+v, want the agent-advertised list", curModel, models)
	}
}

// Fresh sessions retain the FULL advertised config-option set, with a
// mode entry synthesized from SessionModeState when no mode config option
// exists — one uniform knob list regardless of which channel the agent
// used. Grouped values flatten with their group label retained.
func TestBackend_RetainsConfigOptionsOnOpen(t *testing.T) {
	t.Parallel()
	category := sdk.SessionConfigOptionCategory("model")
	scripted := &acptest.ScriptedAgent{}
	scripted.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
		return sdk.NewSessionResponse{
			SessionId: "s-options",
			Modes: &sdk.SessionModeState{
				CurrentModeId: "default",
				AvailableModes: []sdk.SessionMode{
					{Id: "default", Name: "Default"},
					{Id: "plan", Name: "Plan"},
				},
			},
			ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
				Id: "model", Name: "Model", Category: &category, CurrentValue: "sonnet",
				Options: sdk.SessionConfigSelectOptions{Grouped: &sdk.SessionConfigSelectOptionsGrouped{
					{Group: "anthropic", Name: "Anthropic", Options: []sdk.SessionConfigSelectOption{
						{Value: "sonnet", Name: "Sonnet"},
						{Value: "opus", Name: "Opus"},
					}},
				}},
			}}},
		}, nil
	}
	f := newBackendFixture(t, scripted, "")
	if err := f.backend.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	opts := f.backend.ConfigOptions()
	if len(opts) != 2 {
		t.Fatalf("ConfigOptions = %+v, want synthesized mode + model", opts)
	}
	mode := opts[0]
	if mode.ID != "mode" || mode.CurrentValue != "default" || len(mode.Values) != 2 || mode.Values[1].Value != "plan" {
		t.Errorf("synthesized mode option = %+v, want the SessionModeState verbatim", mode)
	}
	model := opts[1]
	if model.ID != "model" || model.CurrentValue != "sonnet" || len(model.Values) != 2 {
		t.Fatalf("model option = %+v, want both grouped values flattened", model)
	}
	if model.Values[0].Group != "Anthropic" || model.Values[1].Value != "opus" {
		t.Errorf("flattened values = %+v, want group label retained", model.Values)
	}
}

// config_option_update notifications carry the FULL replacement set; the
// retained options must follow it, and the synthesized mode entry must
// survive an update that carries no mode option — mode state rides a
// different channel the update never re-sends, so rebuilding from the
// update alone would silently drop the mode knob.
func TestBackend_ConfigOptionUpdateRefreshesRetainedOptions(t *testing.T) {
	t.Parallel()
	category := sdk.SessionConfigOptionCategory("model")
	scripted := &acptest.ScriptedAgent{}
	scripted.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
		return sdk.NewSessionResponse{
			SessionId: "s-update",
			Modes: &sdk.SessionModeState{
				CurrentModeId:  "default",
				AvailableModes: []sdk.SessionMode{{Id: "default", Name: "Default"}},
			},
			ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
				Id: "model", Name: "Model", Category: &category, CurrentValue: "sonnet",
				Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
					{Value: "sonnet", Name: "Sonnet"},
					{Value: "opus", Name: "Opus"},
				}},
			}}},
		}, nil
	}
	f := newBackendFixture(t, scripted, "")
	if err := f.backend.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	f.backend.HandleSessionUpdate(context.Background(), sdk.SessionNotification{
		SessionId: "s-update",
		Update: sdk.SessionUpdate{ConfigOptionUpdate: &sdk.SessionConfigOptionUpdate{
			ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
				Id: "model", Name: "Model", Category: &category, CurrentValue: "opus",
				Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
					{Value: "sonnet", Name: "Sonnet"},
					{Value: "opus", Name: "Opus"},
				}},
			}}},
		}},
	})

	opts := f.backend.ConfigOptions()
	if len(opts) != 2 {
		t.Fatalf("ConfigOptions after update = %+v, want synthesized mode preserved + model", opts)
	}
	if opts[0].ID != "mode" || opts[0].CurrentValue != "default" {
		t.Errorf("mode option after update = %+v, want it preserved from session state", opts[0])
	}
	if opts[1].CurrentValue != "opus" {
		t.Errorf("model current after update = %q, want opus", opts[1].CurrentValue)
	}
	if current, _ := f.backend.Models(); current != "opus" {
		t.Errorf("Models() current after update = %q, want opus (same channel)", current)
	}
}

// ConfigOptions() must hand out a deep copy: mutating a caller's returned
// Values slice must not corrupt the backend's retained state, which a
// concurrent caller (or this same caller on its next probe) still reads.
func TestBackend_ConfigOptionsReturnsIndependentCopy(t *testing.T) {
	t.Parallel()
	category := sdk.SessionConfigOptionCategory("model")
	scripted := &acptest.ScriptedAgent{}
	scripted.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
		return sdk.NewSessionResponse{
			SessionId: "s-clone",
			ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
				Id: "model", Name: "Model", Category: &category, CurrentValue: "sonnet",
				Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
					{Value: "sonnet", Name: "Sonnet"},
					{Value: "opus", Name: "Opus"},
				}},
			}}},
		}, nil
	}
	f := newBackendFixture(t, scripted, "")
	if err := f.backend.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	first := f.backend.ConfigOptions()
	first[0].Values[0].Value = "corrupted"

	second := f.backend.ConfigOptions()
	if second[0].Values[0].Value != "sonnet" {
		t.Fatalf("second ConfigOptions() call = %+v, want unaffected by mutating the first call's result", second[0].Values)
	}
}

// Rejecting a permission with a reason must deliver that reason to the
// agent. ACP permission outcomes carry an option id and nothing else, so
// the text becomes the user's next prompt — which is exactly what makes
// plan revision work: rejecting ExitPlanMode keeps the session in plan mode
// and ends the turn, then "use click instead" arrives and the agent revises.
// Previously the text was logged and dropped, so the comments went nowhere.
func TestBackend_DenyMessageBecomesFollowUpPrompt(t *testing.T) {
	t.Parallel()
	const denyMsg = "No — use click instead of argparse, and add a --quiet flag."

	prompts := make(chan string, 4)
	outcome := make(chan sdk.RequestPermissionResponse, 1)
	var mu sync.Mutex
	turns := 0

	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		text := ""
		for _, blk := range p.Prompt {
			if blk.Text != nil {
				text += blk.Text.Text
			}
		}
		prompts <- text

		mu.Lock()
		turns++
		first := turns == 1
		mu.Unlock()
		if !first {
			// The follow-up turn: the revision request, nothing to ask about.
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		}

		resp, err := scripted.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "tc-plan", Title: sdk.Ptr("Ready to code?")},
			Options: []sdk.PermissionOption{
				{OptionId: "default", Name: "Yes, and manually approve edits", Kind: sdk.PermissionOptionKindAllowOnce},
				{OptionId: "plan", Name: "No, keep planning", Kind: sdk.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return sdk.PromptResponse{}, err
		}
		outcome <- resp
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}

	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "plan the flag"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	evts := f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventPermission) == 1
	})
	var perm agent.PermissionData
	for _, e := range evts {
		if e.Type == agent.EventPermission {
			perm = e.Data.(agent.PermissionData)
		}
	}

	if err := f.backend.RespondPermission(ctx, perm.RequestID, false, denyMsg); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}

	// The agent sees a rejection (stay in plan mode) …
	select {
	case resp := <-outcome:
		if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "plan" {
			t.Fatalf("agent saw outcome %+v, want the reject option", resp.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent never received the permission outcome")
	}

	// … and the reason arrives as the next prompt, not dropped.
	deadline := time.After(5 * time.Second)
	var got []string
	for {
		select {
		case p := <-prompts:
			got = append(got, p)
			if p == denyMsg {
				goto delivered
			}
		case <-deadline:
			t.Fatalf("deny message never reached the agent; prompts seen: %q", got)
		}
	}
delivered:

	// It is also a visible user message, not an invisible side channel.
	f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		for _, e := range evts {
			if e.Type == agent.EventMessage {
				if m, ok := e.Data.(agent.MessageData); ok && m.Role == "user" && m.Content == denyMsg {
					return true
				}
			}
		}
		return false
	})
}

// An approved permission has no reason to carry: a message alongside
// allow=true must NOT become a phantom follow-up prompt (OP-003).
func TestBackend_AllowIgnoresMessage(t *testing.T) {
	t.Parallel()
	prompts := make(chan string, 4)
	var mu sync.Mutex
	turns := 0

	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		text := ""
		for _, blk := range p.Prompt {
			if blk.Text != nil {
				text += blk.Text.Text
			}
		}
		prompts <- text
		mu.Lock()
		turns++
		first := turns == 1
		mu.Unlock()
		if !first {
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		}
		if _, err := scripted.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "tc-1", Title: sdk.Ptr("Run tests")},
			Options: []sdk.PermissionOption{
				{OptionId: "yes", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
				{OptionId: "no", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
			},
		}); err != nil {
			return sdk.PromptResponse{}, err
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}

	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	evts := f.waitFor(5*time.Second, func(evts []agent.Event) bool {
		return countType(evts, agent.EventPermission) == 1
	})
	var perm agent.PermissionData
	for _, e := range evts {
		if e.Type == agent.EventPermission {
			perm = e.Data.(agent.PermissionData)
		}
	}
	if err := f.backend.RespondPermission(ctx, perm.RequestID, true, "ignore me"); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	<-prompts // the original prompt
	select {
	case p := <-prompts:
		t.Fatalf("allow=true sent a follow-up prompt %q; the message must be ignored", p)
	case <-time.After(500 * time.Millisecond):
	}
}

// Config keys beyond the mode ride session/set_config_option — this is
// what makes effort/thought_level reachable at all (the old wire carried
// only a mode). Mode applies first (it can gate what other options mean),
// the rest in sorted order, all before the prompt dispatches.
func TestBackend_SendConfigAppliesModeFirstThenSortedOptions(t *testing.T) {
	t.Parallel()
	type call struct{ kind, id, value string }
	calls := make(chan call, 8)
	desc := "Read-only sandbox"
	scripted := &acptest.ScriptedAgent{}
	scripted.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
		return sdk.NewSessionResponse{
			SessionId: "s-cfg",
			Modes: &sdk.SessionModeState{
				CurrentModeId: "read-only",
				AvailableModes: []sdk.SessionMode{
					{Id: "read-only", Name: "Read Only", Description: &desc},
					{Id: "agent", Name: "Agent"},
				},
			},
		}, nil
	}
	scripted.SetModeFn = func(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
		calls <- call{kind: "mode", value: string(p.ModeId)}
		return sdk.SetSessionModeResponse{}, nil
	}
	scripted.SetConfigFn = func(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
		calls <- call{kind: "config", id: string(p.ValueId.ConfigId), value: string(p.ValueId.Value)}
		return sdk.SetSessionConfigOptionResponse{}, nil
	}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{
		Text: "go",
		Config: map[string]string{
			"reasoning_effort":     "high",
			agent.ConfigOptionMode: "agent",
			"collaboration_mode":   "plan",
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := []call{
		{kind: "mode", value: "agent"},
		{kind: "config", id: "collaboration_mode", value: "plan"},
		{kind: "config", id: "reasoning_effort", value: "high"},
	}
	for i, w := range want {
		select {
		case got := <-calls:
			if got != w {
				t.Fatalf("call[%d] = %+v, want %+v", i, got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("call[%d] never arrived", i)
		}
	}
}

// An empty option id is never something an agent advertises — forwarding
// it would just be a meaningless set_config_option RPC (ConfigId="").
func TestBackend_SendConfigSkipsEmptyOptionID(t *testing.T) {
	t.Parallel()
	type call struct{ kind, id, value string }
	calls := make(chan call, 8)
	scripted := &acptest.ScriptedAgent{}
	scripted.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
		return sdk.NewSessionResponse{SessionId: "s-empty-id"}, nil
	}
	scripted.SetConfigFn = func(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
		calls <- call{kind: "config", id: string(p.ValueId.ConfigId), value: string(p.ValueId.Value)}
		return sdk.SetSessionConfigOptionResponse{}, nil
	}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	f := newBackendFixture(t, scripted, "")
	ctx := context.Background()
	if err := f.backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.backend.Send(ctx, agent.SendMessageOpts{
		Text: "go",
		Config: map[string]string{
			"":       "should-be-skipped",
			"effort": "high",
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case got := <-calls:
		if got.id != "effort" {
			t.Fatalf("call = %+v, want id=effort", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected config call never arrived")
	}
	select {
	case got := <-calls:
		t.Fatalf("unexpected extra call %+v; empty option id must be skipped", got)
	case <-time.After(300 * time.Millisecond):
	}
}
