package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

// ServerResolver returns the current server URL for a project directory.
// It may start a new server if the previous one died. Backends call this
// on reconnect to discover port changes after a server restart.
type ServerResolver func(ctx context.Context) (string, error)

// OpenCodeBackend manages a single OpenCode session via the OpenCode Go SDK.
//
// Architecture:
//   - The daemon manages one OpenCode server per project directory
//     (via OpenCodeServerManager). The backend receives a ServerResolver
//     to dynamically discover the server URL (which may change across restarts).
//   - Each backend instance corresponds to one session on that server.
//   - Events are streamed via SSE from GET /event on the server.
//   - On disconnect, the SSE stream reconnects with exponential backoff,
//     re-resolving the server URL each time (handles port changes).
type OpenCodeBackend struct {
	mu           sync.Mutex
	status       SessionStatus
	sessionID    string         // OpenCode's session ID (assigned by server)
	serverURL    string         // e.g. "http://127.0.0.1:4123"
	resolver     ServerResolver // resolves current server URL (may restart server)
	events       chan Event
	eventOnce    sync.Once // protects close(events)
	eventsClosed bool      // true after events channel is closed
	watchOnce    sync.Once // ensures streamEvents is started at most once
	ctx          context.Context
	cancel       context.CancelFunc
	client       *opencode.Client

	// messageRoles tracks message ID -> role so we can skip user part updates.
	messageRoles sync.Map // map[string]opencode.MessageRole
}

// NewOpenCodeBackend creates a new OpenCode backend that communicates with
// an already-running OpenCode server at the given URL. If sessionID is
// non-empty, the backend is pre-associated with that session (used for
// historical/resume sessions loaded from discover).
//
// The resolver is called on SSE reconnect and API retry to discover the
// current server URL, which may change if the server restarts on a new port.
// If resolver is nil, the backend uses the initial serverURL permanently
// (no reconnect capability).
func NewOpenCodeBackend(serverURL string, sessionID string, resolver ServerResolver) *OpenCodeBackend {
	ctx, cancel := context.WithCancel(context.Background())
	serverURL = strings.TrimRight(serverURL, "/")
	client := opencode.NewClient(option.WithBaseURL(serverURL))
	return &OpenCodeBackend{
		status:    StatusIdle,
		serverURL: serverURL,
		resolver:  resolver,
		sessionID: sessionID,
		events:    make(chan Event, 128),
		ctx:       ctx,
		cancel:    cancel,
		client:    client,
	}
}

func (b *OpenCodeBackend) Start(ctx context.Context, req StartRequest) error {
	if req.SessionID != "" {
		// Resume existing session.
		b.mu.Lock()
		b.sessionID = req.SessionID
		b.mu.Unlock()
	}

	if b.SessionID() == "" {
		// Create new session via SDK.
		session, err := b.client.Session.New(ctx, opencode.SessionNewParams{})
		if err != nil && isConnectionError(err) && b.resolver != nil {
			if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
				session, err = b.client.Session.New(ctx, opencode.SessionNewParams{})
			}
		}
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		b.mu.Lock()
		b.sessionID = session.ID
		b.mu.Unlock()
	}

	// Start SSE event listener in background (idempotent — skips if already watching).
	b.startWatching()

	// Mark as busy BEFORE sending the prompt, so the TUI shows the correct
	// status while the agent is working.
	b.setStatus(StatusBusy)

	// Send the initial prompt via SDK. This blocks until the full response
	// is received, but events stream in via SSE in the background.
	params := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Text: opencode.F(req.Prompt),
				Type: opencode.F(opencode.TextPartInputTypeText),
			},
		}),
	}
	if req.Agent != "" {
		params.Agent = opencode.F(req.Agent)
	}
	_, err := b.client.Session.Prompt(ctx, b.sessionID, params)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.Prompt(ctx, b.sessionID, params)
		}
	}
	if err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("send prompt: %w", err)
	}

	// Don't set status here — the SSE stream will deliver session.idle
	// which triggers the idle transition.
	return nil
}

// Watch starts listening for SSE events on this session without sending a
// prompt. Use this to observe a discovered/historical session that may be
// active. The Events channel will produce events after Watch returns.
func (b *OpenCodeBackend) Watch(ctx context.Context) error {
	if b.SessionID() == "" {
		return fmt.Errorf("cannot watch: session ID not set")
	}
	b.startWatching()
	return nil
}

// startWatching launches the SSE event listener goroutine if not already running.
func (b *OpenCodeBackend) startWatching() {
	b.watchOnce.Do(func() {
		go b.streamEvents()
	})
}

func (b *OpenCodeBackend) SendMessage(ctx context.Context, opts SendMessageOpts) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}

	// Mark as busy BEFORE sending, so the TUI shows the correct status.
	b.setStatus(StatusBusy)

	params := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Text: opencode.F(opts.Text),
				Type: opencode.F(opencode.TextPartInputTypeText),
			},
		}),
	}
	if opts.Agent != "" {
		params.Agent = opencode.F(opts.Agent)
	}
	_, err := b.client.Session.Prompt(ctx, b.sessionID, params)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.Prompt(ctx, b.sessionID, params)
		}
	}
	if err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("send message: %w", err)
	}
	// Don't set status here — SSE session.idle will trigger the transition.
	return nil
}

func (b *OpenCodeBackend) Abort(ctx context.Context) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}
	_, err := b.client.Session.Abort(ctx, b.sessionID, opencode.SessionAbortParams{})
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.Abort(ctx, b.sessionID, opencode.SessionAbortParams{})
		}
	}
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	return nil
}

func (b *OpenCodeBackend) Revert(ctx context.Context, messageID string) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}
	_, err := b.client.Session.Revert(ctx, b.sessionID, opencode.SessionRevertParams{
		MessageID: opencode.F(messageID),
	})
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.Revert(ctx, b.sessionID, opencode.SessionRevertParams{
				MessageID: opencode.F(messageID),
			})
		}
	}
	if err != nil {
		return fmt.Errorf("revert: %w", err)
	}
	return nil
}

func (b *OpenCodeBackend) Fork(ctx context.Context, messageID string) (ForkResult, error) {
	if b.sessionID == "" {
		return ForkResult{}, fmt.Errorf("session not started")
	}

	doFork := func() (ForkResult, error) {
		url := b.serverURL + "/session/" + b.sessionID + "/fork"
		var bodyMap map[string]string
		if messageID != "" {
			bodyMap = map[string]string{"messageID": messageID}
		} else {
			bodyMap = map[string]string{}
		}
		body, err := json.Marshal(bodyMap)
		if err != nil {
			return ForkResult{}, fmt.Errorf("fork: marshal body: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return ForkResult{}, fmt.Errorf("fork: create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return ForkResult{}, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			return ForkResult{}, fmt.Errorf("fork: server returned %d: %s", resp.StatusCode, string(respBody))
		}

		var result struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return ForkResult{}, fmt.Errorf("fork: decode response: %w", err)
		}
		return ForkResult{ID: result.ID, Title: result.Title}, nil
	}

	res, err := doFork()
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			res, err = doFork()
		}
	}
	if err != nil {
		return ForkResult{}, fmt.Errorf("fork: %w", err)
	}
	return res, nil
}

func (b *OpenCodeBackend) Stop() error {
	b.cancel()
	b.closeEvents()
	return nil
}

func (b *OpenCodeBackend) Events() <-chan Event {
	return b.events
}

func (b *OpenCodeBackend) Status() SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *OpenCodeBackend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

func (b *OpenCodeBackend) RespondPermission(ctx context.Context, permissionID string, allow bool) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}
	response := opencode.SessionPermissionRespondParamsResponseOnce
	if !allow {
		response = opencode.SessionPermissionRespondParamsResponseReject
	}
	_, err := b.client.Session.Permissions.Respond(ctx, b.sessionID, permissionID, opencode.SessionPermissionRespondParams{
		Response: opencode.F(response),
	})
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.Permissions.Respond(ctx, b.sessionID, permissionID, opencode.SessionPermissionRespondParams{
				Response: opencode.F(response),
			})
		}
	}
	if err != nil {
		return fmt.Errorf("respond permission: %w", err)
	}
	return nil
}

func (b *OpenCodeBackend) setStatus(s SessionStatus) {
	b.mu.Lock()
	old := b.status
	b.status = s
	b.mu.Unlock()

	if old != s {
		b.emit(Event{
			Type:      EventStatusChange,
			Timestamp: time.Now(),
			Data: StatusChangeData{
				OldStatus: old,
				NewStatus: s,
			},
		})
	}
}

func (b *OpenCodeBackend) closeEvents() {
	b.eventOnce.Do(func() {
		b.mu.Lock()
		b.eventsClosed = true
		b.mu.Unlock()
		close(b.events)
	})
}

func (b *OpenCodeBackend) emit(evt Event) {
	// Recover from send-on-closed-channel if the SSE stream goroutine
	// closed the events channel between our check and the send.
	defer func() { recover() }()

	b.mu.Lock()
	closed := b.eventsClosed
	b.mu.Unlock()
	if closed {
		return
	}
	select {
	case b.events <- evt:
	default:
		// Drop if full.
	}
}

// streamEvents connects to the OpenCode SSE endpoint and reconnects with
// exponential backoff when the connection drops. On each reconnect attempt,
// it re-resolves the server URL via the ServerResolver, which handles
// server restarts (potentially on a new port).
//
// Uses the raw /event endpoint instead of the SDK — see connectAndStreamSSE
// for the rationale.
func (b *OpenCodeBackend) streamEvents() {
	defer b.closeEvents()

	// Backoff schedule: 1s, 2s, 4s, 8s, 16s, then give up.
	backoff := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}

	attempt := 0
	for {
		wasConnected, err := b.connectAndStreamSSE(attempt)

		// Context cancelled = intentional shutdown, exit cleanly.
		if b.ctx.Err() != nil {
			return
		}

		// If we were connected and streaming successfully before the drop,
		// reset the attempt counter — the server was alive, this is a fresh
		// failure (not a continuation of a previous failure).
		if wasConnected {
			attempt = 0
		}

		// No resolver = can't reconnect, give up immediately.
		if b.resolver == nil {
			if err != nil {
				b.emitError("SSE stream: " + err.Error())
			}
			return
		}

		if attempt >= len(backoff) {
			b.emit(Event{
				Type:      EventReconnecting,
				Timestamp: time.Now(),
				Data: ReconnectingData{
					Attempt: attempt + 1,
					GaveUp:  true,
					Error:   fmt.Sprintf("SSE: giving up after %d reconnect attempts", attempt),
				},
			})
			return
		}

		delay := backoff[attempt]
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		b.emit(Event{
			Type:      EventReconnecting,
			Timestamp: time.Now(),
			Data: ReconnectingData{
				Attempt: attempt + 1,
				Delay:   delay,
				Error:   errMsg,
			},
		})

		select {
		case <-time.After(delay):
		case <-b.ctx.Done():
			return
		}

		// Re-resolve server URL before reconnecting. The server may have
		// restarted on a different port.
		urlChanged, resolveErr := b.refreshServerURL()
		if resolveErr != nil {
			b.emitError("SSE reconnect: " + resolveErr.Error())
			attempt++
			continue
		}

		_ = urlChanged // logged by refreshServerURL if needed
		attempt++
	}
}

// connectAndStreamSSE performs a single SSE connection to the OpenCode
// server's /event endpoint and processes events until the stream ends or
// errors. Returns (true, nil) if it received at least one SSE event before
// disconnecting, (false, err) if the connection failed or closed before any
// data arrived. The bool indicates whether the connection was productive
// (used by the caller to reset backoff — a 200+immediate-EOF is not
// considered productive).
//
// A bug was the reason for not using SDK's Event.ListStreaming():
// New event "message.part.delta" is not supported by SDK.
// Similar issue in vibe-kanban upon further research:
// https://github.com/BloopAI/vibe-kanban/issues/3123.
//
// Last update to the Go SDK was 3 months ago, probably causing this
// discrepancy between what the server sends and what the SDK handles.
//
// The server's /event endpoint uses Bus.subscribeAll(), which publishes message.part.delta
// events containing token-level streaming deltas — exactly what we need for
// real-time text display. The SDK's Stream[EventListResponse] either silently
// drops these (unknown union variant) or may break the stream entirely.
func (b *OpenCodeBackend) connectAndStreamSSE(attempt int) (connected bool, err error) {
	b.mu.Lock()
	serverURL := b.serverURL
	b.mu.Unlock()

	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, serverURL+"/event", nil)
	if err != nil {
		return false, fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		select {
		case <-b.ctx.Done():
			return false, b.ctx.Err()
		default:
			return false, fmt.Errorf("SSE connect: %w", err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("SSE endpoint returned %d", resp.StatusCode)
	}

	// Successfully connected.
	if attempt > 0 {
		b.emit(Event{
			Type:      EventReconnected,
			Timestamp: time.Now(),
			Data: ReconnectedData{
				Attempts: attempt,
			},
		})
	}

	// Use bufio.Reader instead of bufio.Scanner to avoid a hard upper
	// limit on line length. Scanner fails permanently with "token too
	// long" when a single SSE data line exceeds its buffer cap (common
	// with large session snapshots for long conversations). ReadBytes
	// allocates dynamically, eliminating this class of failure.
	reader := bufio.NewReader(resp.Body)

	// Track whether we received any meaningful data. Only reset the caller's
	// backoff counter if we actually streamed events (not just 200 + EOF).
	receivedData := false

	var dataBuf bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")

		if len(line) == 0 && err == nil {
			// Empty line = dispatch the accumulated event.
			if dataBuf.Len() > 0 {
				b.handleRawSSEEvent(dataBuf.Bytes())
				dataBuf.Reset()
				receivedData = true
			}
			continue
		}

		if len(line) > 0 {
			// Parse SSE field.
			name, value, _ := bytes.Cut(line, []byte(":"))
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}

			switch string(name) {
			case "data":
				dataBuf.Write(value)
				dataBuf.WriteByte('\n')
			case "":
				// Comment line (": something"), ignore.
			}
		}

		if err != nil {
			// Dispatch any buffered event before returning.
			if dataBuf.Len() > 0 {
				b.handleRawSSEEvent(dataBuf.Bytes())
				dataBuf.Reset()
				receivedData = true
			}
			if err == io.EOF {
				return receivedData, nil // clean close
			}
			select {
			case <-b.ctx.Done():
				return receivedData, b.ctx.Err()
			default:
				return receivedData, fmt.Errorf("SSE stream: %w", err)
			}
		}
	}
}

// partDeltaEvent is the JSON shape of a "message.part.delta" BusEvent as
// sent by the OpenCode server. This event carries token-level streaming
// deltas and is NOT modeled by the Go SDK.
type partDeltaEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		PartID    string `json:"partID"`
		Field     string `json:"field"`
		Delta     string `json:"delta"`
	} `json:"properties"`
}

// permissionAskedEvent is the JSON shape of a "permission.asked" BusEvent
// as sent by the OpenCode server. This event is emitted when the server
// needs user approval for a tool call and is NOT modeled by the Go SDK
// (which only has "permission.updated" / "permission.replied").
type permissionAskedEvent struct {
	Type       string `json:"type"`
	Properties struct {
		ID         string                 `json:"id"`
		Permission string                 `json:"permission"` // e.g. "bash", "write"
		Patterns   []string               `json:"patterns"`   // e.g. ["npx cowsay hello"]
		Always     []string               `json:"always"`     // broader pattern for "always allow"
		SessionID  string                 `json:"sessionID"`
		Metadata   map[string]interface{} `json:"metadata"`
		Tool       struct {
			MessageID string `json:"messageID"`
			CallID    string `json:"callID"`
		} `json:"tool"`
	} `json:"properties"`
}

// handleRawSSEEvent processes a single SSE data payload (raw JSON bytes).
// It handles event types not modeled by the SDK ("message.part.delta",
// "permission.asked") with custom parsing, then falls back to the SDK's
// EventListResponse unmarshalling for all other types.
func (b *OpenCodeBackend) handleRawSSEEvent(data []byte) {
	// Quick peek at the "type" field to decide how to route.
	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return // Malformed JSON, skip.
	}

	switch peek.Type {
	case "message.part.delta":
		var delta partDeltaEvent
		if err := json.Unmarshal(data, &delta); err != nil {
			return
		}
		b.handlePartDelta(delta)
		return

	case "permission.asked":
		// "permission.asked" is emitted when OpenCode needs user approval
		// for a tool call. Not modeled by the SDK — handle directly.
		var asked permissionAskedEvent
		if err := json.Unmarshal(data, &asked); err != nil {
			return
		}
		b.handlePermissionAsked(asked)
		return
	}

	// For all other event types, delegate to the SDK's typed unmarshaller.
	var event opencode.EventListResponse
	if err := json.Unmarshal(data, &event); err != nil {
		// Unknown event type the SDK can't parse — silently skip.
		return
	}
	b.handleSDKEvent(event)
}

// handlePermissionAsked processes a "permission.asked" event from the
// OpenCode server. This event type is not modeled by the SDK.
func (b *OpenCodeBackend) handlePermissionAsked(asked permissionAskedEvent) {
	props := asked.Properties
	if props.SessionID != b.SessionID() {
		return
	}

	// Build a human-readable description from the permission fields.
	// e.g. "bash: npx cowsay hello"
	description := props.Permission
	if len(props.Patterns) > 0 {
		description += ": " + strings.Join(props.Patterns, ", ")
	}

	b.emit(Event{
		Type:      EventPermission,
		Timestamp: time.Now(),
		Data: PermissionData{
			RequestID:   props.ID,
			Tool:        props.Permission,
			Description: description,
		},
	})
}

// handlePartDelta processes a message.part.delta event (token-level streaming).
func (b *OpenCodeBackend) handlePartDelta(delta partDeltaEvent) {
	props := delta.Properties

	// Filter to our session.
	if props.SessionID != b.SessionID() {
		return
	}

	// Skip parts belonging to user messages.
	if role, ok := b.messageRoles.Load(props.MessageID); ok {
		if role == opencode.MessageRoleUser {
			return
		}
	}

	// Only handle text content deltas (field="text" for text parts).
	if props.Delta == "" {
		return
	}

	b.emit(Event{
		Type:      EventPartUpdate,
		Timestamp: time.Now(),
		Data: PartUpdateData{
			Part: Part{
				ID:   props.PartID,
				Type: PartText,
				Text: props.Delta,
			},
			IsDelta: true,
		},
	})
}

// handleSDKEvent translates an OpenCode SDK event into our unified Event type.
func (b *OpenCodeBackend) handleSDKEvent(event opencode.EventListResponse) {
	switch e := event.AsUnion().(type) {
	case opencode.EventListResponseEventSessionIdle:
		if e.Properties.SessionID != b.sessionID {
			return
		}
		b.setStatus(StatusIdle)

	case opencode.EventListResponseEventSessionError:
		if e.Properties.SessionID != "" && e.Properties.SessionID != b.sessionID {
			return
		}
		b.setStatus(StatusError)
		b.emitError(string(e.Properties.Error.Name))

	case opencode.EventListResponseEventMessageUpdated:
		msg := e.Properties.Info
		if msg.SessionID != b.sessionID {
			return
		}
		// Track message ID -> role so we can filter user parts.
		b.messageRoles.Store(msg.ID, msg.Role)

		// Emit user messages so the TUI can backfill the messageID on
		// inline user entries (which are created before the server assigns
		// an ID). The TUI skips rendering but uses the ID for revert.
		// TODO(ae): don't do this async, TUI should get ID from SendMessage instead.
		if msg.Role == opencode.MessageRoleUser {
			b.emit(Event{
				Type:      EventMessage,
				Timestamp: time.Now(),
				Data: MessageData{
					ID:   msg.ID,
					Role: string(msg.Role),
				},
			})
			return
		}

		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data: MessageData{
				Role: string(msg.Role),
			},
		})

	case opencode.EventListResponseEventMessagePartUpdated:
		part := e.Properties.Part
		delta := e.Properties.Delta

		// Filter to our session.
		if part.SessionID != b.sessionID {
			return
		}

		// Skip parts belonging to user messages.
		if role, ok := b.messageRoles.Load(part.MessageID); ok {
			if role == opencode.MessageRoleUser {
				return
			}
		}

		// If the SDK ever populates delta (future SDK version), use it.
		// Currently, token-level deltas arrive via message.part.delta events
		// handled in handlePartDelta, so this is a fallback only.
		if delta != "" {
			b.emit(Event{
				Type:      EventPartUpdate,
				Timestamp: time.Now(),
				Data: PartUpdateData{
					Part: Part{
						ID:   part.ID,
						Type: PartText,
						Text: delta,
					},
					IsDelta: true,
				},
			})
			return
		}

		// Handle full part update based on part type.
		b.handlePartUpdate(part)

	// TODO(opencode-sdk-go#57): This case is likely dead code. The OpenCode server
	// sends "permission.asked" (handled in handleRawSSEEvent), not
	// "permission.updated". Remove once the SDK models permission.asked and
	// this path is confirmed unreachable.
	// https://github.com/anomalyco/opencode-sdk-go/issues/57
	case opencode.EventListResponseEventPermissionUpdated:
		perm := e.Properties
		if perm.SessionID != b.sessionID {
			return
		}
		b.emit(Event{
			Type:      EventPermission,
			Timestamp: time.Now(),
			Data: PermissionData{
				RequestID:   perm.ID,
				Tool:        perm.Type,
				Description: perm.Title,
			},
		})

	case opencode.EventListResponseEventSessionUpdated:
		sess := e.Properties.Info
		if sess.ID != b.sessionID {
			return
		}
		// Emit a title change event when the session title is updated
		// (e.g. by OpenCode's hidden "title" agent after the first response).
		if sess.Title != "" {
			b.emit(Event{
				Type:      EventTitleChange,
				Timestamp: time.Now(),
				Data: TitleChangeData{
					Title: sess.Title,
				},
			})
		}
		// Emit a revert change event so the TUI can filter messages accordingly.
		b.emit(Event{
			Type:      EventRevertChange,
			Timestamp: time.Now(),
			Data: RevertChangeData{
				MessageID: sess.Revert.MessageID,
			},
		})
	}
}

// handlePartUpdate processes a full Part update from the SDK.
func (b *OpenCodeBackend) handlePartUpdate(p opencode.Part) {
	switch concrete := p.AsUnion().(type) {
	case opencode.TextPart:
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:   concrete.ID,
					Type: PartText,
					Text: concrete.Text,
				},
			},
		})

	case opencode.ToolPart:
		var partStatus PartStatus
		switch concrete.State.Status {
		case opencode.ToolPartStateStatusPending:
			partStatus = PartPending
		case opencode.ToolPartStateStatusRunning:
			partStatus = PartRunning
		case opencode.ToolPartStateStatusCompleted:
			partStatus = PartCompleted
		case opencode.ToolPartStateStatusError:
			partStatus = PartFailed
		}
		inputMap, _ := concrete.State.Input.(map[string]interface{})
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:     concrete.ID,
					Type:   PartToolCall,
					Tool:   concrete.Tool,
					Status: partStatus,
					Input:  inputMap,
					Output: concrete.State.Output,
				},
			},
		})

	case opencode.ReasoningPart:
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:   concrete.ID,
					Type: PartThinking,
					Text: concrete.Text,
				},
			},
		})

	default:
		// Skip types we don't care about (StepStartPart, StepFinishPart, etc.).
	}
}

// Messages returns the full message history for this session by calling
// the OpenCode SDK's Session.Messages API and translating to our types.
// On connection errors, it re-resolves the server URL (which may trigger
// a server restart) and retries once.
func (b *OpenCodeBackend) Messages(ctx context.Context) ([]MessageData, error) {
	if b.sessionID == "" {
		return nil, fmt.Errorf("session not started")
	}

	resp, err := b.client.Session.Messages(ctx, b.sessionID, opencode.SessionMessagesParams{})
	if err != nil && isConnectionError(err) && b.resolver != nil {
		// Server may have restarted on a new port. Re-resolve and retry once.
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			resp, err = b.client.Session.Messages(ctx, b.sessionID, opencode.SessionMessagesParams{})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}
	if resp == nil {
		return nil, nil
	}

	var messages []MessageData
	for _, msg := range *resp {
		md := MessageData{
			ID:   msg.Info.ID,
			Role: string(msg.Info.Role),
		}

		for _, p := range msg.Parts {
			converted := b.convertSDKPart(p)
			if converted != nil {
				md.Parts = append(md.Parts, *converted)
			}
		}

		messages = append(messages, md)
	}
	return messages, nil
}

// convertSDKPart translates an OpenCode SDK Part into our Part type.
// Returns nil for part types we don't display (StepStart, StepFinish, etc.).
func (b *OpenCodeBackend) convertSDKPart(p opencode.Part) *Part {
	switch concrete := p.AsUnion().(type) {
	case opencode.TextPart:
		return &Part{
			ID:   concrete.ID,
			Type: PartText,
			Text: concrete.Text,
		}
	case opencode.ToolPart:
		var partStatus PartStatus
		switch concrete.State.Status {
		case opencode.ToolPartStateStatusPending:
			partStatus = PartPending
		case opencode.ToolPartStateStatusRunning:
			partStatus = PartRunning
		case opencode.ToolPartStateStatusCompleted:
			partStatus = PartCompleted
		case opencode.ToolPartStateStatusError:
			partStatus = PartFailed
		}
		inputMap, _ := concrete.State.Input.(map[string]interface{})
		return &Part{
			ID:     concrete.ID,
			Type:   PartToolCall,
			Tool:   concrete.Tool,
			Status: partStatus,
			Input:  inputMap,
			Output: concrete.State.Output,
		}
	case opencode.ReasoningPart:
		return &Part{
			ID:   concrete.ID,
			Type: PartThinking,
			Text: concrete.Text,
		}
	default:
		return nil
	}
}

func (b *OpenCodeBackend) emitError(msg string) {
	b.emit(Event{
		Type:      EventError,
		Timestamp: time.Now(),
		Data:      ErrorData{Message: msg},
	})
}

// refreshServerURL calls the resolver to get the current server URL. If the
// URL changed (e.g. server restarted on a new port), it recreates the SDK
// client. Returns true if the URL changed. Returns an error if resolution fails
// or no resolver is configured.
func (b *OpenCodeBackend) refreshServerURL() (changed bool, err error) {
	if b.resolver == nil {
		return false, fmt.Errorf("no server resolver configured")
	}
	newURL, err := b.resolver(b.ctx)
	if err != nil {
		return false, fmt.Errorf("resolve server URL: %w", err)
	}
	newURL = strings.TrimRight(newURL, "/")

	b.mu.Lock()
	defer b.mu.Unlock()
	if newURL == b.serverURL {
		return false, nil
	}
	b.serverURL = newURL
	b.client = opencode.NewClient(option.WithBaseURL(newURL))
	return true, nil
}

// isConnectionError returns true if the error indicates a network-level
// failure (connection refused, reset, timeout) where retrying with a
// potentially new server URL could succeed.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "dial tcp") ||
		strings.Contains(s, "i/o timeout")
}

// --- OpenCode Server Manager ---
//
// Architecture: Kubernetes-style reconciler pattern.
//
// The server manager owns a "desired set" of project directories that should
// have a running OpenCode server. A single reconcile loop (Run) is the ONLY
// code path that starts or stops servers. All other code (GetOrStartServer,
// CreateBackend, DiscoverSessions, etc.) adds directories to the desired set
// and waits for the reconciler to fulfill the request.
//
// This eliminates the previous bugs where servers were started as side effects
// of 5+ uncoordinated code paths (warmPrimaryAgentCaches, DiscoverSessions,
// CreateBackend, handleListAgents, health loop), causing triple-starts on
// startup and port collisions.

// OpenCodeServer tracks a running `opencode serve` process for a project.
type OpenCodeServer struct {
	URL        string
	ProjectDir string
	Cmd        *exec.Cmd
	StartedAt  time.Time
}

// serverWaiter is a channel that receives a result when the reconciler
// finishes starting (or failing to start) a server for a project dir.
type serverWaiter struct {
	ch chan serverStartResult
}

// serverStartResult holds the outcome of a startServer call.
type serverStartResult struct {
	url string
	err error
}

// OpenCodeServerManager manages one OpenCode server per project directory
// using a reconciler pattern. The reconcile loop (Run) is the single owner
// of server lifecycle — it's the only code that calls startServer.
//
// External callers use:
//   - AddDesired(dir): declare that a server should exist for dir
//   - GetOrStartServer(ctx, dir): get URL, waiting for reconciler if needed
//   - StopAll(): graceful shutdown
type OpenCodeServerManager struct {
	mu      sync.Mutex
	servers map[string]*OpenCodeServer // keyed by project dir; only written by reconciler
	agents  map[string][]AgentInfo     // cached agents per server URL

	// desired is the set of project dirs that should have a running server.
	// Only added to, never removed (servers are removed by StopAll or when
	// a dir is explicitly removed in the future).
	desired map[string]bool

	// waiters are callers blocked in GetOrStartServer waiting for the
	// reconciler to start a server for their project dir. The reconciler
	// notifies all waiters for a dir after attempting to start it.
	waiters map[string][]serverWaiter

	// nudge signals the reconcile loop to run immediately. Buffered so
	// senders never block. The reconciler drains it before each tick.
	nudge chan struct{}

	// startServerFn is the function called by the reconciler to start a
	// server. Defaults to startServer (spawns opencode serve). Replaced
	// in tests for dependency injection.
	startServerFn func(ctx context.Context, projectDir string) (*OpenCodeServer, error)
}

// NewOpenCodeServerManager creates a new server manager. The caller must
// call Run() to start the reconcile loop.
func NewOpenCodeServerManager() *OpenCodeServerManager {
	m := &OpenCodeServerManager{
		servers: make(map[string]*OpenCodeServer),
		agents:  make(map[string][]AgentInfo),
		desired: make(map[string]bool),
		waiters: make(map[string][]serverWaiter),
		nudge:   make(chan struct{}, 1),
	}
	m.startServerFn = m.startServer
	return m
}

// SetStartServerFn replaces the function used to start servers. This is
// intended for testing only — production code uses the default (startServer).
func (m *OpenCodeServerManager) SetStartServerFn(fn func(ctx context.Context, projectDir string) (*OpenCodeServer, error)) {
	m.startServerFn = fn
}

// AddDesired adds project directories to the desired set. The reconciler
// will start servers for any dirs that don't already have one on its next
// tick. Safe to call before Run().
func (m *OpenCodeServerManager) AddDesired(dirs ...string) {
	m.mu.Lock()
	for _, dir := range dirs {
		m.desired[dir] = true
	}
	m.mu.Unlock()

	// Nudge the reconciler to pick up the new dirs immediately.
	select {
	case m.nudge <- struct{}{}:
	default: // already nudged
	}
}

// Run is the reconcile loop. It is the ONLY goroutine that starts servers.
// It runs until ctx is cancelled. Call this once from the daemon's Run().
//
// Behavior:
//  1. On each tick (or nudge), snapshot desired dirs and current servers.
//  2. For each desired dir without a healthy server, start one in parallel.
//  3. Notify any waiters (from GetOrStartServer) with the result.
//  4. Health-check existing servers and remove dead ones (they'll be
//     restarted on the next tick since the dir is still in desired).
func (m *OpenCodeServerManager) Run(ctx context.Context) {
	const reconcileInterval = 5 * time.Second

	// Run the first reconcile immediately on startup for fast server starts.
	m.reconcile(ctx)

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		case <-m.nudge:
			// Drain any additional nudges that piled up.
			for {
				select {
				case <-m.nudge:
				default:
					goto drained
				}
			}
		drained:
			m.reconcile(ctx)
		}
	}
}

// reconcile is one pass of the reconciliation loop. It:
// 1. Health-checks existing servers, kills dead ones.
// 2. Starts servers for desired dirs that don't have a healthy server.
// 3. Notifies waiters with results.
func (m *OpenCodeServerManager) reconcile(ctx context.Context) {
	// --- Phase 1: Health-check existing servers ---
	m.mu.Lock()
	type snapshot struct {
		dir string
		url string
		cmd *exec.Cmd
	}
	existing := make([]snapshot, 0, len(m.servers))
	for dir, srv := range m.servers {
		existing = append(existing, snapshot{dir: dir, url: srv.URL, cmd: srv.Cmd})
	}
	m.mu.Unlock()

	// Health-check WITHOUT holding lock.
	var deadDirs []string
	for _, snap := range existing {
		if !m.HealthCheck(snap.url) {
			log.Printf("[reconciler] server for %s at %s is dead", snap.dir, snap.url)
			if snap.cmd != nil && snap.cmd.Process != nil {
				snap.cmd.Process.Kill()
			}
			deadDirs = append(deadDirs, snap.dir)
		}
	}

	// Remove dead servers under lock.
	if len(deadDirs) > 0 {
		m.mu.Lock()
		for _, dir := range deadDirs {
			delete(m.servers, dir)
		}
		m.mu.Unlock()
	}

	// --- Phase 2: Determine which dirs need a server started ---
	m.mu.Lock()
	var toStart []string
	for dir := range m.desired {
		if _, hasServer := m.servers[dir]; !hasServer {
			toStart = append(toStart, dir)
		}
	}
	m.mu.Unlock()

	if len(toStart) == 0 {
		return
	}

	log.Printf("[reconciler] starting servers for %d dirs: %v", len(toStart), toStart)

	// --- Phase 3: Start servers in parallel ---
	type startResult struct {
		dir string
		srv *OpenCodeServer
		err error
	}
	results := make(chan startResult, len(toStart))

	var wg sync.WaitGroup
	for _, dir := range toStart {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			srv, err := m.startServerFn(ctx, d)
			results <- startResult{dir: d, srv: srv, err: err}
		}(dir)
	}

	// Close results channel when all goroutines finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	// --- Phase 4: Register results and notify waiters ---
	for res := range results {
		m.mu.Lock()
		if res.err == nil {
			m.servers[res.dir] = res.srv
			log.Printf("[reconciler] server for %s ready at %s", res.dir, res.srv.URL)
		} else {
			log.Printf("[reconciler] failed to start server for %s: %v", res.dir, res.err)
		}

		// Notify all waiters for this dir.
		waiters := m.waiters[res.dir]
		delete(m.waiters, res.dir)
		m.mu.Unlock()

		result := serverStartResult{err: res.err}
		if res.err == nil {
			result.url = res.srv.URL
		}
		for _, w := range waiters {
			w.ch <- result
		}
	}
}

// GetOrStartServer returns a running server URL for the given project dir.
// If a healthy server exists, it returns immediately. Otherwise, it adds
// the dir to the desired set, nudges the reconciler, and blocks until the
// reconciler starts the server (or fails).
//
// This method does NOT start servers itself — only the reconciler does.
func (m *OpenCodeServerManager) GetOrStartServer(ctx context.Context, projectDir string) (string, error) {
	// Fast path: healthy server already exists.
	m.mu.Lock()
	if srv, ok := m.servers[projectDir]; ok {
		url := srv.URL
		m.mu.Unlock()

		if m.HealthCheck(url) {
			return url, nil
		}

		// Dead — remove it. The reconciler will restart since dir is
		// still in desired. We could return immediately and wait, but
		// it's better to fall through and register as a waiter so we
		// block until the new server is ready.
		m.mu.Lock()
		if cur, ok := m.servers[projectDir]; ok && cur.URL == url {
			if cur.Cmd != nil && cur.Cmd.Process != nil {
				cur.Cmd.Process.Kill()
			}
			delete(m.servers, projectDir)
		} else if cur, ok := m.servers[projectDir]; ok && cur.URL != url {
			// Another goroutine (or the reconciler) already replaced the
			// server with a new one. Return it directly.
			newURL := cur.URL
			m.mu.Unlock()
			return newURL, nil
		}
		// Fall through with lock held.
	}

	// Lock is held here. Check one more time if a server appeared while we
	// were doing the health check (the reconciler may have started one).
	if srv, ok := m.servers[projectDir]; ok {
		url := srv.URL
		m.mu.Unlock()
		return url, nil
	}

	// Register as a waiter and add to desired set.
	w := serverWaiter{ch: make(chan serverStartResult, 1)}
	m.waiters[projectDir] = append(m.waiters[projectDir], w)
	m.desired[projectDir] = true
	m.mu.Unlock()

	// Nudge the reconciler to handle this immediately.
	select {
	case m.nudge <- struct{}{}:
	default:
	}

	// Wait for the reconciler to start the server.
	select {
	case result := <-w.ch:
		if result.err != nil {
			return "", result.err
		}
		return result.url, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ListServers returns a snapshot of all currently tracked servers.
func (m *OpenCodeServerManager) ListServers() []ServerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	servers := make([]ServerInfo, 0, len(m.servers))
	for _, srv := range m.servers {
		pid := 0
		if srv.Cmd != nil && srv.Cmd.Process != nil {
			pid = srv.Cmd.Process.Pid
		}
		servers = append(servers, ServerInfo{
			URL:        srv.URL,
			ProjectDir: srv.ProjectDir,
			PID:        pid,
			StartedAt:  srv.StartedAt,
		})
	}
	return servers
}

// StopAll stops all managed servers and clears the desired set.
// Any pending waiters are notified with an error.
func (m *OpenCodeServerManager) StopAll() {
	m.mu.Lock()

	// Notify pending waiters that we're shutting down.
	for dir, ws := range m.waiters {
		for _, w := range ws {
			w.ch <- serverStartResult{err: fmt.Errorf("server manager shutting down")}
		}
		delete(m.waiters, dir)
	}

	// Clear desired set so the reconciler won't restart anything.
	for dir := range m.desired {
		delete(m.desired, dir)
	}

	// Stop all servers with graceful shutdown.
	for dir, srv := range m.servers {
		if srv.Cmd != nil && srv.Cmd.Process != nil {
			srv.Cmd.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- srv.Cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				srv.Cmd.Process.Kill()
			}
		}
		delete(m.servers, dir)
	}
	m.mu.Unlock()
}

// startServer spawns `opencode serve` and waits for it to print the listening URL.
func (m *OpenCodeServerManager) startServer(ctx context.Context, projectDir string) (*OpenCodeServer, error) {
	cmd := exec.CommandContext(ctx, "opencode", "serve", "--port=0")
	cmd.Dir = projectDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr // Let stderr pass through for debugging.

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode serve: %w", err)
	}

	// Parse stdout for the listening URL.
	// OpenCode prints: "opencode server listening on http://127.0.0.1:<port>"
	urlCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if idx := strings.Index(line, "http://"); idx >= 0 {
				// Extract URL from the line.
				url := line[idx:]
				// Trim any trailing whitespace or control chars.
				url = strings.TrimSpace(url)
				urlCh <- url
				return
			}
		}
		close(urlCh)
	}()

	select {
	case url, ok := <-urlCh:
		if !ok || url == "" {
			cmd.Process.Kill()
			return nil, fmt.Errorf("opencode serve exited without printing URL")
		}

		// Wait for the server to be fully ready (not just listening).
		// The URL being printed only means the port is bound; the server
		// may still be initializing internally. Poll /global/health to
		// confirm it can actually serve requests before we return.
		log.Printf("[server] %s: port open at %s, waiting for readiness...", projectDir, url)
		if err := m.waitForReady(ctx, url, 10*time.Second); err != nil {
			cmd.Process.Kill()
			return nil, fmt.Errorf("server not ready after startup: %w", err)
		}
		log.Printf("[server] %s: ready at %s", projectDir, url)

		return &OpenCodeServer{
			URL:        url,
			ProjectDir: projectDir,
			Cmd:        cmd,
			StartedAt:  time.Now(),
		}, nil
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		return nil, fmt.Errorf("opencode serve did not start within 15s")
	case <-ctx.Done():
		cmd.Process.Kill()
		return nil, ctx.Err()
	}
}

// waitForReady polls the server's health endpoint until it returns 200 or
// the timeout expires. This ensures the server is fully initialized (not
// just listening on a port) before we hand out its URL.
func (m *OpenCodeServerManager) waitForReady(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if m.HealthCheck(url) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check at %s did not pass within %s", url, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// HealthCheck pings the server's health endpoint.
func (m *OpenCodeServerManager) HealthCheck(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/global/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ListAgents returns the primary, non-hidden agents for the given project
// directory. Results are cached per server (agents don't change during a
// server's lifetime). If the server isn't running yet, it will be started.
func (m *OpenCodeServerManager) ListAgents(ctx context.Context, projectDir string) ([]AgentInfo, error) {
	serverURL, err := m.GetOrStartServer(ctx, projectDir)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if cached, ok := m.agents[serverURL]; ok {
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	agents, err := fetchAgents(ctx, serverURL)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.agents[serverURL] = agents
	m.mu.Unlock()

	return agents, nil
}

// fetchAgents calls GET /agent on the OpenCode server and returns primary,
// non-hidden agents. We make a direct HTTP call instead of using the SDK
// because the SDK's Agent struct doesn't include the "hidden" field.
func fetchAgents(ctx context.Context, serverURL string) ([]AgentInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", serverURL+"/agent", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch agents: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch agents: status %d", resp.StatusCode)
	}

	var raw []AgentInfo
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode agents: %w", err)
	}

	// Filter to primary, non-hidden agents only.
	var result []AgentInfo
	for _, a := range raw {
		if a.Mode == "primary" && !a.Hidden {
			result = append(result, a)
		}
	}
	return result, nil
}

// ListProjects queries the OpenCode server for all known projects.
// The server for seedDir must already be running or will be started.
func (m *OpenCodeServerManager) ListProjects(ctx context.Context, seedDir string) ([]ProjectInfo, error) {
	serverURL, err := m.GetOrStartServer(ctx, seedDir)
	if err != nil {
		return nil, err
	}
	client := opencode.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	projects, err := client.Project.List(ctx, opencode.ProjectListParams{})
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	if projects == nil {
		return nil, nil
	}
	var result []ProjectInfo
	for _, p := range *projects {
		result = append(result, ProjectInfo{ID: p.ID, Worktree: p.Worktree})
	}
	return result, nil
}

// ListSessions queries the OpenCode server for all sessions belonging to
// the given project directory. Child/forked sessions (non-empty ParentID)
// are filtered out.
func (m *OpenCodeServerManager) ListSessions(ctx context.Context, projectDir string) ([]SessionSnapshot, error) {
	serverURL, err := m.GetOrStartServer(ctx, projectDir)
	if err != nil {
		return nil, err
	}
	return m.ListSessionsFromServer(ctx, serverURL)
}

// ListSessionsFromServer queries an already-known server URL for sessions.
// Used by DiscoverSessions to avoid starting new servers per worktree.
func (m *OpenCodeServerManager) ListSessionsFromServer(ctx context.Context, serverURL string) ([]SessionSnapshot, error) {
	client := opencode.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	sessions, err := client.Session.List(ctx, opencode.SessionListParams{})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	if sessions == nil {
		return nil, nil
	}
	var result []SessionSnapshot
	for _, s := range *sessions {
		if s.ParentID != "" {
			continue // Skip child/forked sessions
		}
		result = append(result, SessionSnapshot{
			ID:              s.ID,
			Title:           s.Title,
			Directory:       s.Directory,
			RevertMessageID: s.Revert.MessageID,
			CreatedAt:       time.UnixMilli(int64(s.Time.Created)),
			UpdatedAt:       time.UnixMilli(int64(s.Time.Updated)),
		})
	}
	return result, nil
}
