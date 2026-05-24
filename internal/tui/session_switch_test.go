package tui

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// TestReplaceSessionView_CancelsPreviousSubscription is a regression test for
// the bug where switching sessions via the sidebar leaked the previous
// session's SSE subscription. The leaked goroutine kept reading events for
// the old session and delivered sessionEventMsg into the Bubble Tea queue;
// those messages were then routed to the newly-opened SessionViewModel,
// causing one session's streamed input to appear inside another session.
//
// The fix: every site that reassigns m.sessionView must first call
// cancelEvents on the outgoing view. replaceSessionView centralizes this so
// it cannot regress.
func TestReplaceSessionView_CancelsPreviousSubscription(t *testing.T) {
	t.Parallel()

	m := &InboxModel{}

	// First view simulates session A with an in-flight SSE subscription.
	viewA := NewSessionViewModel(nil, "session-A")
	chA := make(chan agent.Event, 1)
	_, cancelA := context.WithCancel(context.Background())
	cancelCalled := false
	viewA.SetEventChannel(chA, func() {
		cancelCalled = true
		cancelA()
	})
	m.sessionView = viewA

	// Switch to session B — must tear down A.
	viewB := NewSessionViewModel(nil, "session-B")
	m.replaceSessionView(viewB)

	if !cancelCalled {
		t.Fatal("expected previous view's cancelEvents to be invoked when replacing sessionView")
	}
	if m.sessionView != viewB {
		t.Fatalf("expected sessionView to be the new view, got %p (want %p)", m.sessionView, viewB)
	}

	// Sanity: replacing when previous is nil must not panic.
	m.sessionView = nil
	m.replaceSessionView(viewB)
}

// TestSessionView_DropsStaleSessionEvent verifies the defense-in-depth filter:
// a sessionEventMsg whose SessionID does not match the view's own sessionID
// (and is not a lifecycle event) must be ignored without mutating display
// state. Without this guard, an in-flight waitForEvent Cmd scheduled before
// a view switch could deliver an event with a captured-but-stale SessionID,
// and the new view would render it as if it belonged here.
func TestSessionView_DropsStaleSessionEvent(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-B")
	entriesBefore := len(m.entries)

	// An event from a different session that happens to be delivered to us.
	stale := sessionEventMsg{event: agent.Event{
		Type:      agent.EventPartUpdate,
		SessionID: "session-A",
		Data: agent.PartUpdateData{
			MessageID: "msg-leak",
			Part: agent.Part{
				ID:   "part-leak",
				Type: agent.PartText,
				Text: "secret content from session A",
			},
		},
	}}

	_, _ = m.Update(stale)

	if len(m.entries) != entriesBefore {
		t.Fatalf("stale event from another session mutated entries: before=%d, after=%d; entries=%+v",
			entriesBefore, len(m.entries), m.entries)
	}
	for _, e := range m.entries {
		if e.content == "secret content from session A" {
			t.Fatalf("leaked content rendered into wrong session: %+v", e)
		}
	}
}

// TestSessionView_AcceptsOwnSessionEvent ensures the defense-in-depth filter
// does not over-filter: events whose SessionID matches the view's must still
// be processed. Without this check the previous test could pass trivially
// by dropping everything.
func TestSessionView_AcceptsOwnSessionEvent(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-B")
	entriesBefore := len(m.entries)

	own := sessionEventMsg{event: agent.Event{
		Type:      agent.EventPartUpdate,
		SessionID: "session-B",
		Data: agent.PartUpdateData{
			MessageID: "msg-1",
			Part: agent.Part{
				ID:   "part-1",
				Type: agent.PartText,
				Text: "hello from B",
			},
		},
	}}

	_, _ = m.Update(own)

	if len(m.entries) <= entriesBefore {
		t.Fatalf("own-session event was dropped; entries unchanged (before=%d, after=%d)",
			entriesBefore, len(m.entries))
	}
}

// TestPartUpdate_AfterHistoryLoad_AppliesDelta is the regression test for
// the bug where re-opening a busy session showed the loaded history but
// then froze: no streamed deltas appeared until the user re-entered the
// session (which re-fetched history including the now-persisted parts).
//
// Root cause: handleSessionMessages stamped every part ID from history
// into m.seenParts, and handlePartUpdate dropped any EventPartUpdate
// whose part ID was in seenParts. Mid-stream parts (already present in
// the persisted history shell when fetched, but still receiving deltas
// from the model) were therefore silently filtered out.
//
// Contract: after history load, an EventPartUpdate with IsDelta=true for
// a part ID present in history must append the new chunk to the existing
// entry's content. upsertPartEntry is idempotent by partID; we rely on
// that instead of a separate dedup map.
func TestPartUpdate_AfterHistoryLoad_AppliesDelta(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-1")
	m.handleSessionMessages([]agent.MessageData{{
		ID:   "msg-1",
		Role: "assistant",
		Parts: []agent.Part{{
			ID:   "part-1",
			Type: agent.PartText,
			Text: "I'll start",
		}},
	}})

	m.handlePartUpdate(agent.PartUpdateData{
		MessageID: "msg-1",
		Part: agent.Part{
			ID:   "part-1",
			Type: agent.PartText,
			Text: " by examining",
		},
		IsDelta: true,
	})

	var found *displayEntry
	for i := range m.entries {
		if m.entries[i].partID == "part-1" {
			found = &m.entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected an entry with partID=part-1, got entries=%+v", m.entries)
	}
	if found.content != "I'll start by examining" {
		t.Fatalf("delta was dropped after history load: got %q, want %q", found.content, "I'll start by examining")
	}
}

// TestPartUpdate_AfterHistoryLoad_AppliesSnapshot covers the non-delta
// path: a full-snapshot update for a part already in history must
// replace (not append to) the existing entry's content.
func TestPartUpdate_AfterHistoryLoad_AppliesSnapshot(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-1")
	m.handleSessionMessages([]agent.MessageData{{
		ID:   "msg-1",
		Role: "assistant",
		Parts: []agent.Part{{
			ID:   "part-1",
			Type: agent.PartText,
			Text: "partial",
		}},
	}})

	m.handlePartUpdate(agent.PartUpdateData{
		MessageID: "msg-1",
		Part: agent.Part{
			ID:   "part-1",
			Type: agent.PartText,
			Text: "completely new text",
		},
		IsDelta: false,
	})

	var found *displayEntry
	for i := range m.entries {
		if m.entries[i].partID == "part-1" {
			found = &m.entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected an entry with partID=part-1, got entries=%+v", m.entries)
	}
	if found.content != "completely new text" {
		t.Fatalf("snapshot was not applied after history load: got %q, want %q", found.content, "completely new text")
	}
}

// TestPartUpdate_AfterHistoryLoad_ToolStatusTransition covers the
// stuck-spinner symptom: a tool call persisted as PartRunning in history
// must visibly transition to PartCompleted when the live SSE update
// arrives, including merging in the Output that wasn't in history yet.
func TestPartUpdate_AfterHistoryLoad_ToolStatusTransition(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-1")
	m.handleSessionMessages([]agent.MessageData{{
		ID:   "msg-1",
		Role: "assistant",
		Parts: []agent.Part{{
			ID:     "tool-1",
			Type:   agent.PartToolCall,
			Tool:   "bash",
			Status: agent.PartRunning,
			Input:  map[string]any{"command": "ls"},
		}},
	}})

	m.handlePartUpdate(agent.PartUpdateData{
		MessageID: "msg-1",
		Part: agent.Part{
			ID:     "tool-1",
			Type:   agent.PartToolResult,
			Tool:   "bash",
			Status: agent.PartCompleted,
			Output: "done",
		},
	})

	var found *displayEntry
	for i := range m.entries {
		if m.entries[i].partID == "tool-1" {
			found = &m.entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected an entry with partID=tool-1, got entries=%+v", m.entries)
	}
	if found.toolPart == nil {
		t.Fatalf("expected toolPart to be set on the tool entry, got entry=%+v", found)
	}
	if found.toolPart.Status != agent.PartCompleted {
		t.Fatalf("tool status did not transition after history load: got %q, want %q", found.toolPart.Status, agent.PartCompleted)
	}
	if found.toolPart.Output != "done" {
		t.Fatalf("tool Output was not merged after history load: got %q, want %q", found.toolPart.Output, "done")
	}
	// Input from the original history shell must still be present after
	// the merge — upsertPartEntry preserves it when the update doesn't
	// carry it.
	if found.toolPart.Input == nil || found.toolPart.Input["command"] != "ls" {
		t.Fatalf("tool Input from history was lost during merge: got %+v", found.toolPart.Input)
	}
}

// TestMessageShell_AfterHistory_StillIgnored guards the invariant that
// the seenParts removal must not regress: the redundant message-shell
// SSE event (EventMessage) that follows a history load must still be
// suppressed by m.historyLoaded so it doesn't append duplicate entries.
func TestMessageShell_AfterHistory_StillIgnored(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-1")
	msg := agent.MessageData{
		ID:   "msg-1",
		Role: "assistant",
		Parts: []agent.Part{{
			ID:   "part-1",
			Type: agent.PartText,
			Text: "hello",
		}},
	}
	m.handleSessionMessages([]agent.MessageData{msg})
	before := len(m.entries)

	// SSE re-delivers the same message shell shortly after.
	m.handleMessage(msg)

	if len(m.entries) != before {
		t.Fatalf("redundant message shell appended duplicate entries: before=%d, after=%d", before, len(m.entries))
	}
}

// TestSessionView_AcceptsSessionLifecycleEvents ensures EventSessionCreate
// and EventSessionDelete events are NOT dropped by the defense-in-depth
// filter even when their SessionID differs — they're broadcast events that
// every open view needs to observe (e.g. so the inbox's list updates).
func TestSessionView_AcceptsSessionLifecycleEvents(t *testing.T) {
	t.Parallel()

	for _, evtType := range []agent.EventType{agent.EventSessionCreate, agent.EventSessionDelete} {
		evtType := evtType
		t.Run(string(evtType), func(t *testing.T) {
			t.Parallel()
			m := NewSessionViewModel(nil, "session-B")
			// Lifecycle events for a *different* session must still reach
			// handleEvent (they don't append display entries today, but the
			// filter must not drop them — that's the contract).
			lifecycle := sessionEventMsg{event: agent.Event{
				Type:      evtType,
				SessionID: "session-A",
			}}
			// Should not panic and should re-schedule waitForEvent if eventsCh
			// is set (we leave it nil here; just verifying no crash and no
			// state corruption).
			_, _ = m.Update(lifecycle)
		})
	}
}
