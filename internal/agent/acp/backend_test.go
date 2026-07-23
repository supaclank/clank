package acp_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	acpx "github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acp/acptest"
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
	proc, err := acptest.Proc(context.Background(), profile, scripted, t.Logf)
	if err != nil {
		t.Fatalf("acptest.Proc: %v", err)
	}
	t.Cleanup(proc.Stop)
	resolver := func(context.Context) (*acpx.AdapterConn, error) { return proc.Conn, nil }
	f.backend = acpx.NewBackend(profile, "/work", resume, "", "", resolver, t.Logf)
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
		return statusOf(evts) == agent.StatusIdle && countType(evts, agent.EventMessage) >= 3
	})

	// Every event carries the external id.
	for _, e := range evts {
		if e.ExternalID == "" {
			t.Errorf("event %s missing ExternalID", e.Type)
		}
	}
	// The user message precedes busy; committed assistant + carrier follow.
	var roles []string
	for _, e := range evts {
		if e.Type == agent.EventMessage {
			roles = append(roles, e.Data.(agent.MessageData).Role)
		}
	}
	if fmt.Sprint(roles) != "[user assistant user]" {
		t.Errorf("message roles = %v, want [user assistant user] (prompt, reply, tool-result carrier)", roles)
	}
	// Tool call + result share one part id across the two messages.
	var callID, resultID string
	for _, e := range evts {
		if e.Type != agent.EventMessage {
			continue
		}
		md := e.Data.(agent.MessageData)
		for _, p := range md.Parts {
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
	}
	if callID == "" || callID != resultID {
		t.Errorf("tool parts: call=%q result=%q, want equal non-empty", callID, resultID)
	}
	// Streaming deltas arrived before the commit.
	if countDeltas(evts) < 2 {
		t.Errorf("expected streamed text deltas, got %d", countDeltas(evts))
	}
	// Messages() equals the committed transcript shape.
	msgs, err := f.backend.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 3 || msgs[1].Content != "hello world" {
		t.Errorf("Messages = %d entries (assistant content %q), want 3 with 'hello world'", len(msgs), msgs[1].Content)
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
		return statusOf(evts) == agent.StatusIdle
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
		return statusOf(evts) == agent.StatusIdle
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
		return statusOf(evts) == agent.StatusError && countType(evts, agent.EventError) >= 1
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
		return statusOf(evts) == agent.StatusError && countType(evts, agent.EventError) >= 1
	})
	if countType(evts, agent.EventError) == 0 {
		t.Errorf("turn error after a failed no-op Abort was swallowed; got %s", eventTypes(evts))
	}
}

// Stop's sweep is the pure path (no session/cancel involved): the agent
// must observe the swept cancelled outcome itself.
func TestBackend_StopSweepsPendingPermission(t *testing.T) {
	t.Parallel()
	outcome := make(chan sdk.RequestPermissionResponse, 1)
	scripted := &acptest.ScriptedAgent{}
	scripted.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		resp, err := scripted.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "tc-2"},
			Options:   []sdk.PermissionOption{{OptionId: "yes", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce}},
		})
		if err == nil {
			outcome <- resp
		}
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
	case resp := <-outcome:
		if resp.Outcome.Cancelled == nil {
			t.Fatalf("swept permission outcome = %+v, want cancelled", resp.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending permission was not swept on stop")
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
	evts := f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return statusOf(evts) == agent.StatusIdle
	})
	// The drained completion must be part of the committed transcript.
	committed := false
	for _, e := range evts {
		if e.Type != agent.EventMessage {
			continue
		}
		md := e.Data.(agent.MessageData)
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
			t.Errorf("post-idle turn-scoped update leaked: %s", e.Type)
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
		return statusOf(evts) == agent.StatusError && countType(evts, agent.EventError) >= 1
	})
	fail = false
	if err := f.backend.Send(ctx, agent.SendMessageOpts{Text: "again"}); err != nil {
		t.Fatalf("Send after error: %v", err)
	}
	f.waitFor(10*time.Second, func(evts []agent.Event) bool {
		return statusOf(evts) == agent.StatusIdle
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
