package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	opencode "github.com/acksell/opencode-go-sdk/sdk"
	"github.com/acksell/opencode-go-sdk/sdk/client"
	"github.com/acksell/opencode-go-sdk/sdk/option"
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
	openMu       sync.Mutex     // serializes Open() so check-and-create is atomic
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
	client       *client.Client

	// messageRoles tracks message ID -> role ("user" or "assistant") so we can
	// skip user part updates. Reverts to plain string once the SDK regen exposes
	// MessageUnion's role as a typed enum on both variants.
	messageRoles sync.Map // map[string]string
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
	return &OpenCodeBackend{
		status:    StatusIdle,
		serverURL: serverURL,
		resolver:  resolver,
		sessionID: sessionID,
		events:    make(chan Event, 128),
		ctx:       ctx,
		cancel:    cancel,
		client:    client.NewClient(option.WithBaseURL(serverURL)),
	}
}

// Open establishes (or re-attaches to) the OpenCode session and starts
// the SSE event stream. If the constructor was given a sessionID, that
// existing session is reattached; otherwise a new session is created via
// Session.New(). Idempotent — repeat calls are no-ops once the SSE
// listener is running. openMu serializes the check-and-create so two
// concurrent callers can't both observe an empty sessionID and create
// duplicate remote sessions.
func (b *OpenCodeBackend) Open(ctx context.Context) error {
	b.openMu.Lock()
	defer b.openMu.Unlock()

	if b.SessionID() == "" {
		// Create new session via SDK.
		session, err := b.client.Session.Create(ctx, &opencode.SessionCreateRequest{})
		if err != nil && isConnectionError(err) && b.resolver != nil {
			if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
				session, err = b.client.Session.Create(ctx, &opencode.SessionCreateRequest{})
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
	return nil
}

// OpenAndSend opens the session and dispatches the initial prompt.
// Send is fire-and-forget (see Send), so this returns once the SSE
// listener is up and the prompt goroutine has been scheduled.
func (b *OpenCodeBackend) OpenAndSend(ctx context.Context, opts SendMessageOpts) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	return b.Send(ctx, opts)
}

// Send dispatches a prompt to an already-Open session. Uses the server's
// async endpoint (POST /session/{id}/prompt_async, HTTP 204): the HTTP call
// returns as soon as the server has queued the prompt, even if the upstream
// LLM stream stalls. Conversation progress is observed via SSE events
// (StatusBusy → text/tool parts → StatusIdle). Errors surface as EventError
// + StatusError, mirroring the Claude backend's structurally async Query.
func (b *OpenCodeBackend) Send(ctx context.Context, opts SendMessageOpts) error {
	if b.SessionID() == "" {
		return fmt.Errorf("session not open")
	}

	b.setStatus(StatusBusy)
	params := b.buildPromptParams(opts)
	sid := b.SessionID()
	go b.runPrompt(sid, params)
	return nil
}

func (b *OpenCodeBackend) buildPromptParams(opts SendMessageOpts) *opencode.SessionPromptAsyncRequest {
	req := &opencode.SessionPromptAsyncRequest{
		SessionID: b.SessionID(),
		// Fern discriminates this union by which variant pointer is non-nil
		// and injects the "type": "text" tag at marshal time. No explicit
		// Type field to set here.
		Parts: []*opencode.SessionPromptAsyncRequestPartsItem{
			{Text: &opencode.TextPartInput{Text: opts.Text}},
		},
	}
	if opts.Agent != "" {
		req.Agent = opencode.String(opts.Agent)
	}
	if opts.Model != nil {
		req.Model = &opencode.SessionPromptAsyncRequestModel{
			ProviderID: opts.Model.ProviderID,
			ModelID:    opts.Model.ModelID,
		}
	}
	return req
}

// runPrompt dispatches Session.PromptAsync and translates errors into a
// status transition + EventError. PromptAsync hits /session/{id}/prompt_async,
// which returns 204 immediately once the server queues the prompt — no
// HTTP-level hang if the upstream LLM stream stalls. Streaming progress
// arrives via SSE on /global/event.
func (b *OpenCodeBackend) runPrompt(sid string, req *opencode.SessionPromptAsyncRequest) {
	err := b.client.Session.PromptAsync(b.ctx, req)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			err = b.client.Session.PromptAsync(b.ctx, req)
		}
	}
	if err != nil {
		// Don't surface ctx-cancelled errors from Stop().
		if b.ctx.Err() != nil {
			return
		}
		b.setStatus(StatusError)
		b.emitError(fmt.Sprintf("send prompt: %v", err))
	}
}

// startWatching launches the SSE event listener goroutine if not already running.
func (b *OpenCodeBackend) startWatching() {
	b.watchOnce.Do(func() {
		go b.streamEvents()
	})
}

func (b *OpenCodeBackend) Abort(ctx context.Context) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}
	_, err := b.client.Session.Abort(ctx, &opencode.SessionAbortRequest{SessionID: b.sessionID})
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.Abort(ctx, &opencode.SessionAbortRequest{SessionID: b.sessionID})
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
	req := &opencode.SessionRevertRequest{
		SessionID: b.sessionID,
		MessageID: messageID,
	}
	_, err := b.client.Session.Revert(ctx, req)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.Revert(ctx, req)
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
	req := &opencode.SessionForkRequest{SessionID: b.sessionID}
	if messageID != "" {
		req.MessageID = opencode.String(messageID)
	}

	session, err := b.client.Session.Fork(ctx, req)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			session, err = b.client.Session.Fork(ctx, req)
		}
	}
	if err != nil {
		return ForkResult{}, fmt.Errorf("fork: %w", err)
	}
	return ForkResult{ID: session.ID, Title: session.Title}, nil
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
	response := opencode.PermissionRespondRequestResponseOnce
	if !allow {
		response = opencode.PermissionRespondRequestResponseReject
	}
	req := &opencode.PermissionRespondRequest{
		SessionID:    b.sessionID,
		PermissionID: permissionID,
		Response:     response,
	}
	_, err := b.client.Session.PermissionRespond(ctx, req)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Session.PermissionRespond(ctx, req)
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
	// Stamp the backend's native session ID on every event so the
	// host→hub HTTP boundary can propagate it without bespoke signalling.
	// See Event.ExternalID docstring.
	if evt.ExternalID == "" {
		evt.ExternalID = b.sessionID
	}
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
// Uses client.Global.Event(ctx), which is the SDK's typed wrapper over the
// /global/event SSE endpoint. The SDK handles SSE framing, payload-envelope
// unwrapping, and union typing; we just iterate.
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

// connectAndStreamSSE opens a single SSE connection via client.Global.Event
// and processes events until the stream ends or errors. Returns (true, nil)
// if it received at least one event before disconnecting, (false, err) if
// the connection failed or closed before any data arrived. The bool tells
// the caller whether to reset its backoff counter (a 200+immediate-EOF is
// not considered productive).
//
// The SDK's typed stream gives us back *opencode.GlobalEvent — the
// envelope ({directory, project, payload}) is already unwrapped into the
// struct, and Payload is a tagged-union with one pointer field per variant.
// Domain events have a non-nil Event* variant; sync-replay duplicates have a
// SyncEvent* variant (we drop those).
func (b *OpenCodeBackend) connectAndStreamSSE(attempt int) (connected bool, err error) {
	// Fern's default SSE buffer cap is 1MB. opencode session-snapshot events
	// (especially after long conversations or large tool outputs) routinely
	// exceed that. Bump to 16MB — well above any realistic single-event size.
	stream, err := b.client.Global.Event(b.ctx, option.WithMaxStreamBufSize(16*1024*1024))
	if err != nil {
		select {
		case <-b.ctx.Done():
			return false, b.ctx.Err()
		default:
			return false, fmt.Errorf("SSE connect: %w", err)
		}
	}
	defer stream.Close()

	// We've connected to a 200-OK stream at this point (SDK would have
	// returned an error otherwise). Only emit Reconnected once we've
	// actually received an event — see receivedData below.
	receivedData := false

	for {
		ev, err := stream.Recv()
		if err != nil {
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
		if !receivedData && attempt > 0 {
			b.emit(Event{
				Type:      EventReconnected,
				Timestamp: time.Now(),
				Data:      ReconnectedData{Attempts: attempt},
			})
		}
		receivedData = true
		b.handleGlobalEvent(&ev)
	}
}

// handleGlobalEvent dispatches a single decoded /global/event payload to the
// clank Event channel. The SDK's GlobalEventPayload is a discriminated
// union — `Type` carries the wire `type` value, and exactly one
// variant pointer is non-nil. All other variants (TUI, MCP, server.*,
// installation, PTY, …) are intentionally ignored.
func (b *OpenCodeBackend) handleGlobalEvent(ev *opencode.GlobalEvent) {
	if ev == nil || ev.Payload == nil {
		return
	}
	p := ev.Payload

	switch {
	case p.MessagePartDelta != nil:
		b.handlePartDelta(p.MessagePartDelta.Properties)

	case p.PermissionAsked != nil:
		// EventPermissionAsked.Properties is a *PermissionRequest directly.
		b.handlePermissionAsked(p.PermissionAsked.Properties)

	case p.SessionIdle != nil:
		if p.SessionIdle.Properties.SessionID != b.sessionID {
			return
		}
		b.setStatus(StatusIdle)

	case p.SessionError != nil:
		props := p.SessionError.Properties
		if props == nil {
			return
		}
		sid := ""
		if props.SessionID != nil {
			sid = *props.SessionID
		}
		if sid != "" && sid != b.sessionID {
			return
		}
		b.setStatus(StatusError)
		if props.Error != nil {
			b.emitError(props.Error.Name)
		}

	case p.MessageUpdated != nil:
		b.handleMessageUpdated(p.MessageUpdated.Properties)

	case p.MessagePartUpdated != nil:
		b.handleMessagePartUpdated(p.MessagePartUpdated.Properties)

	case p.SessionUpdated != nil:
		b.handleSessionUpdated(p.SessionUpdated.Properties)
	}
}

// handlePartDelta processes a message.part.delta event (token-level streaming).
func (b *OpenCodeBackend) handlePartDelta(props *opencode.EventMessagePartDeltaProperties) {
	if props == nil || props.SessionID != b.SessionID() {
		return
	}
	// Skip parts belonging to user messages.
	if role, ok := b.messageRoles.Load(props.MessageID); ok {
		if role == "user" {
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

// handlePermissionAsked processes a permission.asked event from /global/event.
func (b *OpenCodeBackend) handlePermissionAsked(req *opencode.PermissionRequest) {
	if req == nil || req.SessionID != b.SessionID() {
		return
	}
	// Build a human-readable description, e.g. "bash: npx cowsay hello".
	description := req.Permission
	if len(req.Patterns) > 0 {
		description += ": " + strings.Join(req.Patterns, ", ")
	}
	b.emit(Event{
		Type:      EventPermission,
		Timestamp: time.Now(),
		Data: PermissionData{
			RequestID:   req.ID,
			Tool:        req.Permission,
			Description: description,
		},
	})
}

// handleMessageUpdated emits user / assistant Message events from a
// message.updated event. The Message union has Role + User / Assistant
// variants; only one is non-nil.
func (b *OpenCodeBackend) handleMessageUpdated(props *opencode.EventMessageUpdatedProperties) {
	if props == nil || props.Info == nil || props.SessionID != b.sessionID {
		return
	}
	msg := props.Info

	switch {
	case msg.User != nil:
		// Track message ID -> role so we can filter user parts.
		b.messageRoles.Store(msg.User.ID, "user")
		// Emit user messages so the TUI can backfill the messageID on
		// inline user entries (created before the server assigns an ID).
		// TODO(ae): don't do this async, TUI should get ID from SendMessage instead.
		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data: MessageData{
				ID:   msg.User.ID,
				Role: "user",
			},
		})

	case msg.Assistant != nil:
		a := msg.Assistant
		b.messageRoles.Store(a.ID, "assistant")
		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data: MessageData{
				Role:       "assistant",
				ModelID:    a.ModelID,
				ProviderID: a.ProviderID,
			},
		})
	}
}

// handleMessagePartUpdated processes a message.part.updated event by
// translating the Part union into a clank PartUpdate event.
func (b *OpenCodeBackend) handleMessagePartUpdated(props *opencode.EventMessagePartUpdatedProperties) {
	if props == nil || props.Part == nil || props.SessionID != b.sessionID {
		return
	}
	part := props.Part
	// Skip parts belonging to user messages.
	messageID := partMessageID(part)
	if role, ok := b.messageRoles.Load(messageID); ok {
		if role == "user" {
			return
		}
	}
	// Token-level deltas arrive via message.part.delta (see handlePartDelta).
	if converted := b.convertSDKPart(part); converted != nil {
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data:      PartUpdateData{Part: *converted},
		})
	}
}

// handleSessionUpdated emits title + revert change events from session.updated.
func (b *OpenCodeBackend) handleSessionUpdated(props *opencode.EventSessionUpdatedProperties) {
	if props == nil || props.Info == nil || props.Info.ID != b.sessionID {
		return
	}
	sess := props.Info
	// Title can change when OpenCode's hidden "title" agent finishes (after
	// the first prompt response).
	if sess.Title != "" {
		b.emit(Event{
			Type:      EventTitleChange,
			Timestamp: time.Now(),
			Data:      TitleChangeData{Title: sess.Title},
		})
	}
	revertID := ""
	if sess.Revert != nil {
		revertID = sess.Revert.MessageID
	}
	b.emit(Event{
		Type:      EventRevertChange,
		Timestamp: time.Now(),
		Data:      RevertChangeData{MessageID: revertID},
	})
}

// partMessageID extracts the messageID off whichever Part variant is set.
// All response-side Part variants carry it on their concrete type.
func partMessageID(p *opencode.Part) string {
	switch {
	case p.Text != nil:
		return p.Text.MessageID
	case p.Tool != nil:
		return p.Tool.MessageID
	case p.Reasoning != nil:
		return p.Reasoning.MessageID
	case p.File != nil:
		return p.File.MessageID
	case p.Subtask != nil:
		return p.Subtask.MessageID
	case p.Agent != nil:
		return p.Agent.MessageID
	case p.StepStart != nil:
		return p.StepStart.MessageID
	case p.StepFinish != nil:
		return p.StepFinish.MessageID
	case p.Snapshot != nil:
		return p.Snapshot.MessageID
	case p.Patch != nil:
		return p.Patch.MessageID
	case p.Retry != nil:
		return p.Retry.MessageID
	case p.Compaction != nil:
		return p.Compaction.MessageID
	}
	return ""
}

// Messages returns the full message history for this session by calling
// the OpenCode SDK's Session.Messages API and translating to our types.
// On connection errors, it re-resolves the server URL (which may trigger
// a server restart) and retries once.
func (b *OpenCodeBackend) Messages(ctx context.Context) ([]MessageData, error) {
	if b.sessionID == "" {
		return nil, fmt.Errorf("session not started")
	}

	req := &opencode.SessionMessagesRequest{SessionID: b.sessionID}
	resp, err := b.client.Session.Messages(ctx, req)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		// Server may have restarted on a new port. Re-resolve and retry once.
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			resp, err = b.client.Session.Messages(ctx, req)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}

	var messages []MessageData
	for _, msg := range resp {
		md := messageDataFromInfo(msg.Info)
		for _, p := range msg.Parts {
			if converted := b.convertSDKPart(p); converted != nil {
				md.Parts = append(md.Parts, *converted)
			}
		}
		messages = append(messages, md)
	}
	return messages, nil
}

// messageDataFromInfo translates the Message union into a clank MessageData,
// pulling per-variant fields (role, modelID, providerID, id) off whichever
// variant is set.
func messageDataFromInfo(info *opencode.Message) MessageData {
	if info == nil {
		return MessageData{}
	}
	switch {
	case info.Assistant != nil:
		a := info.Assistant
		return MessageData{
			ID:         a.ID,
			Role:       "assistant",
			ModelID:    a.ModelID,
			ProviderID: a.ProviderID,
		}
	case info.User != nil:
		return MessageData{
			ID:   info.User.ID,
			Role: "user",
		}
	}
	return MessageData{}
}

// convertSDKPart translates an SDK Part into clank's Part type, or returns
// nil for variants clank doesn't render (step-start, step-finish, file,
// agent, subtask, snapshot, patch, retry, compaction).
func (b *OpenCodeBackend) convertSDKPart(p *opencode.Part) *Part {
	if p == nil {
		return nil
	}
	switch {
	case p.Text != nil:
		return &Part{
			ID:   p.Text.ID,
			Type: PartText,
			Text: p.Text.Text,
		}
	case p.Tool != nil:
		return convertToolPart(p.Tool)
	case p.Reasoning != nil:
		return &Part{
			ID:   p.Reasoning.ID,
			Type: PartThinking,
			Text: p.Reasoning.Text,
		}
	}
	return nil
}

// convertToolPart maps the ToolState union into clank's flat
// PartStatus + input/output fields.
func convertToolPart(t *opencode.ToolPart) *Part {
	out := &Part{
		ID:   t.ID,
		Type: PartToolCall,
		Tool: t.Tool,
	}
	if t.State == nil {
		return out
	}
	switch {
	case t.State.Pending != nil:
		out.Status = PartPending
		out.Input = t.State.Pending.Input
	case t.State.Running != nil:
		out.Status = PartRunning
		out.Input = t.State.Running.Input
	case t.State.Completed != nil:
		out.Status = PartCompleted
		out.Input = t.State.Completed.Input
		out.Output = t.State.Completed.Output
	case t.State.Error != nil:
		out.Status = PartFailed
		out.Input = t.State.Error.Input
		out.Output = t.State.Error.Error
	}
	return out
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
	b.client = client.NewClient(option.WithBaseURL(newURL))
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
	models  map[string][]ModelInfo     // cached models per server URL

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
		models:  make(map[string][]ModelInfo),
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

// RestartAllServers kills every running OpenCode server, clears all
// cached agents/models, and blocks until the reconciler has every
// previously-desired dir healthy again. Returns the first error
// encountered (if any) but always tries to restart every server first.
//
// Used by the auth wrapper after writing auth.json: OpenCode reads
// provider credentials at process start and never re-reads them, so
// adding/removing a provider requires a process restart. Project dirs
// share a global SQLite DB, so we restart all of them rather than
// trying to identify which dirs need the new auth.
//
// Active sessions are interrupted — any in-flight prompt/tool call
// dies with the process. The reconnect loop in OpenCodeBackend
// re-resolves the new server URL via the resolver closure passed to
// CreateBackend, so historical session state survives.
func (m *OpenCodeServerManager) RestartAllServers(ctx context.Context) error {
	m.mu.Lock()
	dirs := make([]string, 0, len(m.servers))
	for dir, srv := range m.servers {
		if srv.Cmd != nil && srv.Cmd.Process != nil {
			srv.Cmd.Process.Kill()
		}
		delete(m.agents, srv.URL)
		delete(m.models, srv.URL)
		delete(m.servers, dir)
		dirs = append(dirs, dir)
	}
	m.mu.Unlock()

	// Nudge the reconciler so the dirs (still in m.desired) get
	// restarted on the next tick rather than waiting for the 5s
	// reconcileInterval.
	select {
	case m.nudge <- struct{}{}:
	default:
	}

	// Block until each dir has a healthy server again. GetOrStartServer
	// registers as a waiter against the in-flight reconciler tick.
	var firstErr error
	for _, dir := range dirs {
		if _, err := m.GetOrStartServer(ctx, dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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

// fetchAgents calls /agent on the OpenCode server via the SDK and returns
// only primary, non-hidden agents (those the user would actually pick).
func fetchAgents(ctx context.Context, serverURL string) ([]AgentInfo, error) {
	sdk := client.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	agents, err := sdk.Instance.AppAgents(ctx, &opencode.AppAgentsRequest{})
	if err != nil {
		return nil, fmt.Errorf("fetch agents: %w", err)
	}
	var result []AgentInfo
	for _, a := range agents {
		hidden := a.Hidden != nil && *a.Hidden
		if string(a.Mode) != "primary" || hidden {
			continue
		}
		desc := ""
		if a.Description != nil {
			desc = *a.Description
		}
		result = append(result, AgentInfo{
			Name:        a.Name,
			Description: desc,
			Mode:        string(a.Mode),
			Hidden:      hidden,
		})
	}
	return result, nil
}

// ListModels returns available models from connected providers for the given
// project directory. Results are cached per server (providers don't change
// during a server's lifetime). If the server isn't running yet, it will be started.
func (m *OpenCodeServerManager) ListModels(ctx context.Context, projectDir string) ([]ModelInfo, error) {
	serverURL, err := m.GetOrStartServer(ctx, projectDir)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if cached, ok := m.models[serverURL]; ok {
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	models, err := fetchModels(ctx, serverURL)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.models[serverURL] = models
	m.mu.Unlock()

	return models, nil
}

// fetchModels calls /config/providers via the SDK and flattens all models
// from connected providers into a flat list.
func fetchModels(ctx context.Context, serverURL string) ([]ModelInfo, error) {
	sdk := client.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	resp, err := sdk.Config.Providers(ctx, &opencode.ConfigProvidersRequest{})
	if err != nil {
		return nil, fmt.Errorf("fetch providers: %w", err)
	}
	if resp == nil {
		return nil, nil
	}
	var result []ModelInfo
	for _, provider := range resp.Providers {
		// Skip providers with no models (not connected).
		if len(provider.Models) == 0 {
			continue
		}
		for _, model := range provider.Models {
			result = append(result, ModelInfo{
				ID:           model.ID,
				Name:         model.Name,
				ProviderID:   provider.ID,
				ProviderName: provider.Name,
			})
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
	sdk := client.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	projects, err := sdk.Project.List(ctx, &opencode.ProjectListRequest{})
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	var result []ProjectInfo
	for _, p := range projects {
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

// sessionListLimit is the page size used when querying the opencode
// /session endpoint. opencode defaults to 100, which silently truncates for
// users with hundreds of sessions per project. We pick a value comfortably
// above any realistic single-project session count; if a user ever exceeds
// this we should switch to proper pagination. Verified end-to-end by
// TestDiscoverSessions_PaginatesPastDefaultLimit.
const sessionListLimit = 100000.0

// ListSessionsFromServer queries an already-known server URL for sessions.
// Used by DiscoverSessions to avoid starting new servers per worktree.
//
// Note: opencode's HTTP /session API is project-scoped to the server's
// startup directory, even though the underlying SQLite DB is global. To
// list sessions across all projects, callers must hit one server per
// project worktree (see DiscoverSessions).
func (m *OpenCodeServerManager) ListSessionsFromServer(ctx context.Context, serverURL string) ([]SessionSnapshot, error) {
	sdk := client.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	limit := sessionListLimit
	sessions, err := sdk.Session.List(ctx, &opencode.SessionListRequest{Limit: &limit})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var result []SessionSnapshot
	for _, s := range sessions {
		if s.ParentID != nil && *s.ParentID != "" {
			continue // Skip subtask sessions
		}
		revertMessageID := ""
		if s.Revert != nil {
			revertMessageID = s.Revert.MessageID
		}
		snap := SessionSnapshot{
			ID:              s.ID,
			Title:           s.Title,
			Directory:       s.Directory,
			RevertMessageID: revertMessageID,
		}
		if s.Time != nil {
			snap.CreatedAt = time.UnixMilli(int64(s.Time.Created))
			snap.UpdatedAt = time.UnixMilli(int64(s.Time.Updated))
		}
		result = append(result, snap)
	}
	return result, nil
}
