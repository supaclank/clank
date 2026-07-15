package daemoncli

// Pending-question replay coverage: the SSE stream is live-only, so the host
// replays still-pending question events into every new subscription. Without
// this, closing and reopening a session view while the agent waits on an
// AskUserQuestion / opencode question loses the prompt card entirely.

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func questionEventFixture(requestID string) agent.Event {
	return agent.Event{
		Type:      agent.EventQuestion,
		Timestamp: time.Now(),
		Data: agent.QuestionData{
			RequestID: requestID,
			ToolUseID: "toolu_1",
			Questions: []agent.Question{{
				Text:        "Which auth method should we use?",
				Header:      "Auth",
				AllowCustom: true,
				Options: []agent.QuestionOption{
					{Label: "JWT"},
					{Label: "Sessions"},
				},
			}},
		},
	}
}

// A subscriber that connects while a question is pending must receive the
// question event replayed; once the question resolves, later subscribers
// must not.
func TestEventReplay_PendingQuestionOnSubscribe(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A live subscriber acts as the relay barrier: once it observes an event,
	// the host has tracked it (tracking happens before broadcast).
	live, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe (live): %v", err)
	}

	info, b := td.CreateOpenCodeSession(t, "hello")
	go b.PushEvent(questionEventFixture("req-1"))
	if evt, _ := receiveEventsByType(t, live, agent.EventQuestion, 2*time.Second); evt == nil {
		t.Fatal("live subscriber never saw the question event")
	}

	// A fresh subscription — the "user reopened the chat" case — must get the
	// pending question replayed even though it fired before the subscribe.
	replayed, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe (replay): %v", err)
	}
	evt, _ := receiveEventsByType(t, replayed, agent.EventQuestion, 2*time.Second)
	if evt == nil {
		t.Fatal("new subscriber did not receive the pending question replay")
	}
	data, ok := evt.Data.(agent.QuestionData)
	if !ok {
		t.Fatalf("replayed Data type = %T, want QuestionData", evt.Data)
	}
	if data.RequestID != "req-1" || evt.SessionID != info.ID {
		t.Errorf("replayed question = %+v (session %s), want req-1 on %s", data, evt.SessionID, info.ID)
	}

	// Resolving retires it from the snapshot: no replay for later subscribers.
	go b.PushEvent(agent.Event{
		Type:      agent.EventQuestionResolved,
		Timestamp: time.Now(),
		Data:      agent.QuestionResolvedData{RequestID: "req-1"},
	})
	if evt, _ := receiveEventsByType(t, live, agent.EventQuestionResolved, 2*time.Second); evt == nil {
		t.Fatal("live subscriber never saw the resolved event")
	}

	after, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe (after resolve): %v", err)
	}
	if evt, _ := receiveEventsByType(t, after, agent.EventQuestion, 500*time.Millisecond); evt != nil {
		t.Errorf("resolved question was still replayed: %+v", evt.Data)
	}
}
