package agent_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// These tests replay a provider-level turn failure the way opencode actually
// emits it on /global/event, captured verbatim from opencode 1.15.1 with a
// github-copilot 401 (AuthenticateToken authentication failed). The bug they
// pin down: clank surfaced neither EventError nor a terminal status — the
// session stayed "busy" forever and every client spun on "thinking".
//
// Two independent failure ingredients are covered:
//
//  1. Poison frames. opencode emits session.next.{agent,model}.switched with
//     a string timestamp while the spec (and thus the SDK type) says number.
//     One undecodable frame must drop only itself — killing the SSE stream
//     loses every event until reconnect, including the terminal
//     session.error/session.idle that arrive a few hundred ms later.
//  2. Error mapping. session.error must surface as EventError with the
//     provider detail (not just the union name) plus a terminal StatusError
//     that the trailing session.idle does not clear.

// poisonAgentSwitchFrame is a verbatim session.next.agent.switched frame from
// opencode 1.15.1 (string timestamp, fails float64 decode in the SDK).
func poisonAgentSwitchFrame(sessionID string) string {
	return fmt.Sprintf(`{"directory":"/private/tmp/test","project":"global","payload":{"type":"session.next.agent.switched","properties":{"sessionID":"%s","timestamp":"2026-07-12T22:57:34.385Z","agent":"build"},"id":"evt_poison1"}}`, sessionID)
}

func poisonModelSwitchFrame(sessionID string) string {
	return fmt.Sprintf(`{"directory":"/private/tmp/test","project":"global","payload":{"type":"session.next.model.switched","properties":{"sessionID":"%s","timestamp":"2026-07-12T22:57:34.386Z","providerID":"github-copilot","modelID":"claude-sonnet-4.6"},"id":"evt_poison2"}}`, sessionID)
}

// copilot401ErrorProperties is the session.error payload opencode 1.15.1
// published for the github-copilot 401, as persisted on the failed assistant
// message (trimmed headers).
func copilot401ErrorProperties(sessionID string) map[string]interface{} {
	return map[string]interface{}{
		"sessionID": sessionID,
		"error": map[string]interface{}{
			"name": "APIError",
			"data": map[string]interface{}{
				"message":      "Unauthorized: unauthorized: unauthorized: AuthenticateToken authentication failed",
				"statusCode":   401,
				"isRetryable":  false,
				"responseBody": "unauthorized: unauthorized: AuthenticateToken authentication failed\n",
				"metadata": map[string]interface{}{
					"url": "https://api.githubcopilot.com/chat/completions",
				},
			},
		},
	}
}

func writeRawSSEFrame(w http.ResponseWriter, flusher http.Flusher, frame string) {
	fmt.Fprintf(w, "data: %s\n\n", frame)
	flusher.Flush()
}

// sentinelTitle marks the end of a replayed frame sequence. session.updated
// emits EventTitleChange, so once the collector sees it, every prior frame
// has been processed — no sleep-based settling.
const sentinelTitle = "sentinel-all-frames-processed"

func writeSentinelFrame(w http.ResponseWriter, flusher http.Flusher, sessionID string) {
	writeSSEEvent(w, flusher, "session.updated", map[string]interface{}{
		"sessionID": sessionID,
		"info": map[string]interface{}{
			"id":        sessionID,
			"slug":      "test",
			"version":   "1.15.1",
			"projectID": "global",
			"directory": "/private/tmp/test",
			"path":      "private/tmp/test",
			"title":     sentinelTitle,
			"cost":      0,
			"tokens":    map[string]interface{}{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
			"time":      map[string]interface{}{"created": 1783896791714, "updated": 1783896791714},
		},
	})
}

// collectEventsUntilTitle drains the event channel until the sentinel
// EventTitleChange arrives (or the timeout expires).
func collectEventsUntilTitle(t *testing.T, ch <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	var events []agent.Event
	timer := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, evt)
			if evt.Type == agent.EventTitleChange {
				if data, ok := evt.Data.(agent.TitleChangeData); ok && data.Title == sentinelTitle {
					return events
				}
			}
		case <-timer:
			t.Fatalf("timed out waiting for sentinel title event; got %d events: %+v", len(events), events)
			return events
		}
	}
}

// TestOpenCodeBackendSSEProviderErrorSurfaces replays the full captured turn:
// poison frames, session.status busy, session.error (APIError 401),
// session.status, session.idle. The backend must emit EventError carrying the
// provider detail and land on StatusError — with the trailing session.idle
// NOT clearing the error status.
func TestOpenCodeBackendSSEProviderErrorSurfaces(t *testing.T) {
	t.Parallel()
	mock := newMockOpenCodeServer()
	defer mock.Close()

	sseReady := make(chan string, 1)
	mock.setSSEHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)
		flusher.Flush()

		var sessionID string
		select {
		case sessionID = <-sseReady:
		case <-r.Context().Done():
			return
		}

		// The turn as opencode 1.15.1 emitted it (order preserved from the
		// live capture).
		writeRawSSEFrame(w, flusher, poisonAgentSwitchFrame(sessionID))
		writeRawSSEFrame(w, flusher, poisonModelSwitchFrame(sessionID))
		writeSSEEvent(w, flusher, "session.status", map[string]interface{}{
			"sessionID": sessionID,
			"status":    map[string]interface{}{"type": "busy"},
		})
		writeSSEEvent(w, flusher, "session.error", copilot401ErrorProperties(sessionID))
		writeSSEEvent(w, flusher, "session.status", map[string]interface{}{
			"sessionID": sessionID,
			"status":    map[string]interface{}{"type": "idle"},
		})
		writeSSEEvent(w, flusher, "session.idle", map[string]interface{}{
			"sessionID": sessionID,
		})
		writeSentinelFrame(w, flusher, sessionID)

		<-r.Context().Done()
	})

	b := agent.NewOpenCodeBackend(mock.URL(), "", nil)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "hello"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	sseReady <- b.SessionID()

	events := collectEventsUntilTitle(t, b.Events(), 5*time.Second)

	var errEvent *agent.ErrorData
	var sawBusyToError bool
	for _, evt := range events {
		switch data := evt.Data.(type) {
		case agent.ErrorData:
			d := data
			errEvent = &d
		case agent.StatusChangeData:
			if data.OldStatus == agent.StatusBusy && data.NewStatus == agent.StatusError {
				sawBusyToError = true
			}
			if data.NewStatus == agent.StatusIdle && data.OldStatus == agent.StatusError {
				t.Errorf("session.idle after session.error must not clear the error status, got %s->%s", data.OldStatus, data.NewStatus)
			}
		}
	}

	if errEvent == nil {
		t.Fatalf("expected EventError, got none; events: %+v", events)
	}
	if !strings.Contains(errEvent.Message, "AuthenticateToken authentication failed") {
		t.Errorf("EventError must carry the provider detail, got %q", errEvent.Message)
	}
	if !strings.Contains(errEvent.Message, "APIError") {
		t.Errorf("EventError must carry the error name, got %q", errEvent.Message)
	}
	if !sawBusyToError {
		t.Errorf("expected StatusChange busy->error, events: %+v", events)
	}
	if got := b.Status(); got != agent.StatusError {
		t.Errorf("expected terminal status=error, got %s", got)
	}
}

// TestOpenCodeBackendSSEAbortedErrorReturnsToIdle pins the abort semantics:
// a user-initiated abort surfaces as session.error{MessageAbortedError} and
// must return the session to Idle without emitting EventError — expected
// fallout of the interrupt, not a failure (mirrors the Claude backend).
func TestOpenCodeBackendSSEAbortedErrorReturnsToIdle(t *testing.T) {
	t.Parallel()
	mock := newMockOpenCodeServer()
	defer mock.Close()

	sseReady := make(chan string, 1)
	mock.setSSEHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)
		flusher.Flush()

		var sessionID string
		select {
		case sessionID = <-sseReady:
		case <-r.Context().Done():
			return
		}

		writeSSEEvent(w, flusher, "session.error", map[string]interface{}{
			"sessionID": sessionID,
			"error": map[string]interface{}{
				"name": "MessageAbortedError",
				"data": map[string]interface{}{"message": "aborted"},
			},
		})
		writeSSEEvent(w, flusher, "session.idle", map[string]interface{}{
			"sessionID": sessionID,
		})
		writeSentinelFrame(w, flusher, sessionID)

		<-r.Context().Done()
	})

	b := agent.NewOpenCodeBackend(mock.URL(), "", nil)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "hello"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	sseReady <- b.SessionID()

	events := collectEventsUntilTitle(t, b.Events(), 5*time.Second)

	for _, evt := range events {
		if evt.Type == agent.EventError {
			t.Errorf("abort must not surface EventError, got %+v", evt.Data)
		}
	}
	if got := b.Status(); got != agent.StatusIdle {
		t.Errorf("expected status=idle after abort, got %s", got)
	}
}

// TestOpenCodeBackendSSEUndecodableFrameKeepsStream guarantees the skip
// contract independent of SDK evolution: a frame whose payload hard-fails
// GlobalEvent decode (sessionID as a number) must be dropped alone — events
// after it on the same connection still flow. Guards against the "one poison
// frame kills the stream and the terminal events land in the reconnect
// blackout" failure mode for FUTURE spec drift, whatever shape it takes.
func TestOpenCodeBackendSSEUndecodableFrameKeepsStream(t *testing.T) {
	t.Parallel()
	mock := newMockOpenCodeServer()
	defer mock.Close()

	sseReady := make(chan string, 1)
	mock.setSSEHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)
		flusher.Flush()

		var sessionID string
		select {
		case sessionID = <-sseReady:
		case <-r.Context().Done():
			return
		}

		// sessionID typed as number — undecodable against the SDK's string
		// field regardless of SDK version.
		writeRawSSEFrame(w, flusher, `{"directory":"/private/tmp/test","project":"global","payload":{"type":"session.idle","properties":{"sessionID":12345},"id":"evt_bad"}}`)
		writeSSEEvent(w, flusher, "session.idle", map[string]interface{}{
			"sessionID": sessionID,
		})
		writeSentinelFrame(w, flusher, sessionID)

		<-r.Context().Done()
	})

	b := agent.NewOpenCodeBackend(mock.URL(), "", nil)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "hello"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	sseReady <- b.SessionID()

	events := collectEventsUntilTitle(t, b.Events(), 5*time.Second)

	// The valid session.idle after the poison frame must have flipped
	// busy->idle on the SAME connection (no reconnect events in between).
	for _, evt := range events {
		if evt.Type == agent.EventReconnecting || evt.Type == agent.EventReconnected {
			t.Errorf("undecodable frame must not drop the connection, saw %s", evt.Type)
		}
	}
	if got := b.Status(); got != agent.StatusIdle {
		t.Errorf("expected status=idle from the frame after the poison one, got %s", got)
	}
}
