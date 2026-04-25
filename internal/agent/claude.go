package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// activeToolBlock tracks metadata for an in-progress tool_use block so that
// handleContentBlockStop can emit a PartCompleted event with the correct ID
// and tool name (the TUI replaces the entire toolPart on upsert).
type activeToolBlock struct {
	partID   string
	tool     string
	inputBuf strings.Builder // accumulates input_json_delta chunks
}

// ClaudeCodeBackend manages a single Claude Code session using the
// claude-agent-sdk-go SDK's Client API. The SDK handles CLI discovery,
// subprocess lifecycle, JSON parsing, streaming, and the control protocol.
//
// Architecture:
//   - Each backend instance corresponds to one session.
//   - Connect() spawns a persistent Claude CLI subprocess.
//   - Multi-turn: Query() sends follow-up prompts over the same connection.
//   - Abort uses the SDK's control protocol (Interrupt), not raw SIGINT.
//   - receiveLoop maps SDK messages → clank Event types.
//   - Messages() reads the full history from Claude's on-disk JSONL transcript
//     via the SDK's GetSessionMessages, so history survives daemon restarts.
type ClaudeCodeBackend struct {
	mu         sync.Mutex
	openMu     sync.Mutex // serializes Open() so check-and-create is atomic
	status     SessionStatus
	sessionID  string // Claude's CLI session UUID (from ResultMessage)
	projectDir string
	events     chan Event
	stopped    bool // guards against double-close of events channel
	ctx        context.Context
	cancel     context.CancelFunc

	client claudecode.Client // SDK client (persistent connection)

	// currentMsgID is the Anthropic API message ID (e.g. "msg_01XFD...") extracted
	// from the most recent message_start stream event. It's used to build part IDs
	// for text/thinking blocks as "{msgID}-{blockIndex}".
	//
	// This is naturally unique across both message cycles within a turn (each tool
	// use triggers a new API call with a new message ID) and across turns (each
	// Query() produces new API calls). No synthetic counters needed.
	//
	// Only accessed from receiveLoop goroutine — no lock required.
	currentMsgID string

	// activeToolBlocks maps block index → tool metadata for the current message
	// cycle. Populated by handleContentBlockStart for tool_use blocks, consumed
	// by handleContentBlockStop to emit PartCompleted with the correct ID and
	// tool name (fixing the stuck spinner and blank tool label).
	// Reset on each message_start since block indices restart at 0 per cycle.
	// Only accessed from receiveLoop goroutine — no lock required.
	activeToolBlocks map[int]activeToolBlock

	// ClientFactory builds a claudecode.Client for a given set of options.
	// Tests inject a factory that returns a client backed by a mock transport.
	// If nil, the default claudecode.NewClient is used.
	ClientFactory func(opts ...claudecode.Option) claudecode.Client
}

// NewClaudeCodeBackend creates a new Claude Code backend. workDir is
// the host-resolved working directory (worktree or repo root) the
// claude CLI will be launched in.
func NewClaudeCodeBackend(workDir string) *ClaudeCodeBackend {
	return NewClaudeCodeBackendForSession(workDir, "")
}

// NewClaudeCodeBackendForSession is the resume variant. It pre-seeds the
// SDK session ID so that Messages() can read the on-disk JSONL transcript
// immediately, and so that Open() launches the CLI with --resume to
// reattach the persistent subprocess for the existing conversation.
// resumeSessionID may be empty for fresh sessions.
func NewClaudeCodeBackendForSession(workDir, resumeSessionID string) *ClaudeCodeBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &ClaudeCodeBackend{
		status:           StatusStarting,
		projectDir:       workDir,
		sessionID:        resumeSessionID,
		events:           make(chan Event, 128),
		activeToolBlocks: make(map[int]activeToolBlock),
		ctx:              ctx,
		cancel:           cancel,
	}
}

// Open spawns the Claude CLI subprocess (via the SDK) and starts the
// receiveLoop. If the constructor was given a resumeSessionID, the CLI
// is launched with --resume so the JSONL transcript is reattached.
//
// Idempotent: a second call while already connected returns nil. openMu
// serializes the check-and-create so concurrent callers can't both spawn
// a CLI subprocess and orphan one of them.
func (b *ClaudeCodeBackend) Open(ctx context.Context) error {
	b.openMu.Lock()
	defer b.openMu.Unlock()

	b.mu.Lock()
	if b.client != nil {
		b.mu.Unlock()
		return nil
	}
	workDir := b.projectDir
	resumeID := b.sessionID
	factory := b.ClientFactory
	b.mu.Unlock()

	// Defensive guard: an empty workDir would silently inherit the
	// daemon's cwd via claudecode.WithCwd(""), which usually means a
	// caller forgot to populate ProjectDir before constructing the
	// backend. Fail fast instead of running the agent against the
	// wrong tree.
	if workDir == "" {
		return fmt.Errorf("claude backend: project dir is empty; refuse to inherit daemon cwd")
	}

	opts := []claudecode.Option{
		claudecode.WithCwd(workDir),
		claudecode.WithPartialStreaming(),
		claudecode.WithPermissionMode(claudecode.PermissionModeAcceptEdits),
	}
	if resumeID != "" {
		opts = append(opts, claudecode.WithResume(resumeID))
	}

	// Build the client — use the test factory if provided.
	var client claudecode.Client
	if factory != nil {
		client = factory(opts...)
	} else {
		client = claudecode.NewClient(opts...)
	}

	// Only commit b.client after a successful Connect, so a failed Open
	// leaves the backend retryable instead of stuck in a half-open state.
	if err := client.Connect(b.ctx); err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("connect to claude CLI: %w", err)
	}

	b.mu.Lock()
	b.client = client
	b.mu.Unlock()

	// Connection established. Transition out of StatusStarting so the
	// TUI's spinner clears for re-attached sessions (those that only
	// call Open without a follow-up Send/OpenAndSend). For the create
	// path, OpenAndSend has already flipped status to Busy before
	// calling us, so this conditional leaves it alone.
	b.mu.Lock()
	if b.status == StatusStarting {
		b.mu.Unlock()
		b.setStatus(StatusIdle)
	} else {
		b.mu.Unlock()
	}

	// Start receiving messages from the SDK in the background.
	go b.receiveLoop()
	return nil
}

// OpenAndSend opens the session and dispatches the initial prompt. The
// CLI subprocess is connected before the prompt is queued so the
// receiveLoop is in place to observe SystemMessage{init} (which carries
// the external session ID).
//
// Unlike Send, OpenAndSend does NOT emit an EventMessage{Role:user} for
// the prompt: the hub already persists the initial prompt out-of-band
// when creating the session, so emitting one here would duplicate it in
// the TUI history. Follow-up prompts go through Send and DO emit the
// user message because there's no other place that records them.
//
// Query is dispatched on b.ctx (not the caller's ctx) so the prompt is
// tied to backend lifetime, not request lifetime, mirroring OpenCode's
// Send. The actual LLM response streams via receiveLoop on b.ctx, so
// honoring the caller's ctx for the synchronous handoff would only
// surface "cancel mid-enqueue" as a spurious StatusError without
// stopping the work that has already started.
func (b *ClaudeCodeBackend) OpenAndSend(ctx context.Context, opts SendMessageOpts) error {
	// Mark Busy before Open so Open's "Starting → Idle" conditional
	// is bypassed on the create path; we go directly Starting → Busy.
	b.setStatus(StatusBusy)

	if err := b.Open(ctx); err != nil {
		return err
	}

	b.mu.Lock()
	client := b.client
	b.mu.Unlock()
	if client == nil {
		return fmt.Errorf("session not open after Open")
	}

	if err := client.Query(b.ctx, opts.Text); err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("send initial prompt: %w", err)
	}
	return nil
}

// Send dispatches a prompt to an already-Open session. Query runs on
// b.ctx (not the caller's ctx) — see OpenAndSend's doc for the rationale.
func (b *ClaudeCodeBackend) Send(ctx context.Context, opts SendMessageOpts) error {
	b.mu.Lock()
	client := b.client
	b.mu.Unlock()

	if client == nil {
		return fmt.Errorf("session not open: client not connected")
	}

	// Emit user message event so the TUI sees it.
	b.emit(Event{
		Type:      EventMessage,
		Timestamp: time.Now(),
		Data: MessageData{
			Role:    "user",
			Content: opts.Text,
		},
	})

	b.setStatus(StatusBusy)

	if err := client.Query(b.ctx, opts.Text); err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("send prompt: %w", err)
	}

	return nil
}

func (b *ClaudeCodeBackend) Abort(ctx context.Context) error {
	b.mu.Lock()
	client := b.client
	b.mu.Unlock()

	if client == nil {
		return fmt.Errorf("session not started")
	}

	return client.Interrupt(ctx)
}

func (b *ClaudeCodeBackend) Stop() error {
	b.cancel()

	b.mu.Lock()
	client := b.client
	alreadyStopped := b.stopped
	b.stopped = true
	b.mu.Unlock()

	if client != nil {
		client.Disconnect()
	}

	if !alreadyStopped {
		close(b.events)
	}
	return nil
}

func (b *ClaudeCodeBackend) Events() <-chan Event {
	return b.events
}

func (b *ClaudeCodeBackend) Status() SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *ClaudeCodeBackend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// Messages returns the conversation history for this session by reading
// Claude Code's on-disk JSONL transcript via the SDK's GetSessionMessages.
// History therefore survives daemon restarts and matches what `claude --resume`
// would replay.
//
// Returns nil, nil before the SDK has assigned a session ID (i.e. before the
// first ResultMessage / system init has landed). The caller can call this
// again once a session ID is available.
func (b *ClaudeCodeBackend) Messages(ctx context.Context) ([]MessageData, error) {
	b.mu.Lock()
	sessionID := b.sessionID
	workDir := b.projectDir
	b.mu.Unlock()

	if sessionID == "" {
		return nil, nil
	}

	opts := []claudecode.SessionOption{}
	if workDir != "" {
		opts = append(opts, claudecode.WithSessionDirectory(workDir))
	}

	sdkMsgs, err := claudecode.GetSessionMessages(sessionID, opts...)
	if err != nil {
		return nil, fmt.Errorf("read claude session %s: %w", sessionID, err)
	}

	out := make([]MessageData, 0, len(sdkMsgs))
	for _, m := range sdkMsgs {
		md, ok := sessionMessageToData(m)
		if !ok {
			continue
		}
		out = append(out, md)
	}
	return out, nil
}

func (b *ClaudeCodeBackend) Revert(ctx context.Context, messageID string) error {
	return fmt.Errorf("revert is not supported by Claude Code backend")
}

func (b *ClaudeCodeBackend) Fork(ctx context.Context, messageID string) (ForkResult, error) {
	return ForkResult{}, fmt.Errorf("fork is not supported by Claude Code backend")
}

func (b *ClaudeCodeBackend) RespondPermission(ctx context.Context, permissionID string, allow bool) error {
	return fmt.Errorf("permissions are not supported by Claude Code backend")
}

// --- Internal helpers ---

func (b *ClaudeCodeBackend) setStatus(s SessionStatus) {
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

func (b *ClaudeCodeBackend) emit(evt Event) {
	b.mu.Lock()
	stopped := b.stopped
	// Stamp the backend's native session ID on every event so the
	// host→hub HTTP boundary can propagate it without bespoke signalling.
	// Empty until the first SystemMessage{init} arrives; once set it
	// rides every subsequent event for free.
	if evt.ExternalID == "" {
		evt.ExternalID = b.sessionID
	}
	b.mu.Unlock()

	if stopped {
		return
	}

	select {
	case b.events <- evt:
	default:
		// Drop if buffer full — avoids blocking the receive loop.
	}
}

func (b *ClaudeCodeBackend) emitError(msg string) {
	b.emit(Event{
		Type:      EventError,
		Timestamp: time.Now(),
		Data:      ErrorData{Message: msg},
	})
}

// receiveLoop reads messages from the SDK's ReceiveMessages channel and
// translates them into clank Event types. It runs for the lifetime of the
// client connection.
func (b *ClaudeCodeBackend) receiveLoop() {
	msgChan := b.client.ReceiveMessages(b.ctx)

	for msg := range msgChan {
		if msg == nil {
			continue
		}

		switch m := msg.(type) {
		case *claudecode.SystemMessage:
			b.handleSystemMessage(m)
		case *claudecode.AssistantMessage:
			b.handleAssistantMessage(m)
		case *claudecode.ResultMessage:
			b.handleResult(m)
		case *claudecode.StreamEvent:
			b.handleStreamEvent(m)
		}
	}

	// Channel closed — connection ended.
	if b.Status() == StatusBusy || b.Status() == StatusStarting {
		b.setStatus(StatusDead)
	}
}

func (b *ClaudeCodeBackend) handleSystemMessage(m *claudecode.SystemMessage) {
	// The init message carries the session ID in SystemMessage.Data.
	// Once stored, every subsequent emit() stamps it onto Event.ExternalID
	// so the hub captures it the moment any event flows.
	if m.Subtype == "init" {
		if sid, ok := m.Data["session_id"].(string); ok && sid != "" {
			b.mu.Lock()
			b.sessionID = sid
			b.mu.Unlock()
		}
	}
}

func (b *ClaudeCodeBackend) handleAssistantMessage(m *claudecode.AssistantMessage) {
	// Emit a content-less shell — matching the OpenCode pattern.
	// The TUI ignores EventMessage content after history loads, and new
	// content arrives exclusively via EventPartUpdate from streaming deltas
	// (handleContentBlockStart/Delta). Emitting parts here would duplicate
	// what the streaming path already delivered.
	//
	// Full message content (including parts) is reconstructed on demand by
	// Messages() reading the on-disk JSONL transcript via the SDK.
	b.emit(Event{
		Type:      EventMessage,
		Timestamp: time.Now(),
		Data: MessageData{
			Role: "assistant",
		},
	})
}

func (b *ClaudeCodeBackend) handleResult(m *claudecode.ResultMessage) {
	// The result carries the authoritative CLI session UUID.
	if m.SessionID != "" {
		b.mu.Lock()
		b.sessionID = m.SessionID
		b.mu.Unlock()
	}

	if m.IsError {
		errMsg := "unknown error"
		if m.Result != nil {
			errMsg = *m.Result
		}
		b.emitError(errMsg)
		b.setStatus(StatusError)
	} else {
		b.setStatus(StatusIdle)
	}
}

// handleStreamEvent processes partial streaming updates (content_block_start,
// content_block_delta, content_block_stop) and tracks the current Anthropic
// message ID from message_start events.
func (b *ClaudeCodeBackend) handleStreamEvent(m *claudecode.StreamEvent) {
	eventType, _ := m.Event["type"].(string)

	switch eventType {
	case claudecode.StreamEventTypeMessageStart:
		// Extract the Anthropic API message ID (e.g. "msg_01XFD...") from the
		// nested message object. Each API call produces a unique message ID,
		// so this changes on every message cycle (including within a single turn
		// when tool use triggers additional API calls).
		if msgData, ok := m.Event["message"].(map[string]any); ok {
			if msgID, ok := msgData["id"].(string); ok {
				b.currentMsgID = msgID
			}
		}
		// Reset activeToolBlocks — block indices restart at 0 in each message cycle.
		b.activeToolBlocks = make(map[int]activeToolBlock)
	case claudecode.StreamEventTypeContentBlockStart:
		b.handleContentBlockStart(m.Event)
	case claudecode.StreamEventTypeContentBlockDelta:
		b.handleContentBlockDelta(m.Event)
	case claudecode.StreamEventTypeContentBlockStop:
		b.handleContentBlockStop(m.Event)
	}
}

// blockID returns a part ID scoped to the current Anthropic message and block index.
// The message ID (from message_start) is naturally unique across message cycles
// and turns, so no synthetic counters are needed. This mirrors how OpenCode uses
// server-assigned part IDs — we use the API's own message ID as the scope.
func (b *ClaudeCodeBackend) blockID(index int) string {
	// currentMsgID is only read/written from receiveLoop (single goroutine),
	// so no lock is needed here.
	return fmt.Sprintf("%s-%d", b.currentMsgID, index)
}

func (b *ClaudeCodeBackend) handleContentBlockStart(event map[string]any) {
	block, ok := event["content_block"].(map[string]any)
	if !ok {
		return
	}

	index := intFromAny(event["index"])
	blockType, _ := block["type"].(string)

	switch blockType {
	case "text":
		text, _ := block["text"].(string)
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:   b.blockID(index),
					Type: PartText,
					Text: text,
				},
			},
		})
	case "tool_use":
		id, _ := block["id"].(string)
		name, _ := block["name"].(string)
		// Track this tool_use block so handleContentBlockStop can emit PartCompleted
		// with the correct tool name.
		b.activeToolBlocks[index] = activeToolBlock{partID: id, tool: name}
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:     id,
					Type:   PartToolCall,
					Tool:   name,
					Status: PartRunning,
				},
			},
		})
	case "thinking":
		text, _ := block["thinking"].(string)
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:   b.blockID(index),
					Type: PartThinking,
					Text: text,
				},
			},
		})
	}
}

func (b *ClaudeCodeBackend) handleContentBlockDelta(event map[string]any) {
	delta, ok := event["delta"].(map[string]any)
	if !ok {
		return
	}

	index := intFromAny(event["index"])
	deltaType, _ := delta["type"].(string)

	switch deltaType {
	case "text_delta":
		text, _ := delta["text"].(string)
		if text != "" {
			b.emit(Event{
				Type:      EventPartUpdate,
				Timestamp: time.Now(),
				Data: PartUpdateData{
					Part: Part{
						ID:   b.blockID(index),
						Type: PartText,
						Text: text,
					},
					IsDelta: true,
				},
			})
		}
	case "thinking_delta":
		text, _ := delta["thinking"].(string)
		if text != "" {
			b.emit(Event{
				Type:      EventPartUpdate,
				Timestamp: time.Now(),
				Data: PartUpdateData{
					Part: Part{
						ID:   b.blockID(index),
						Type: PartThinking,
						Text: text,
					},
					IsDelta: true,
				},
			})
		}
	case "input_json_delta":
		// Accumulate tool input JSON incrementally so it's available at
		// content_block_stop (and ultimately in the Part.Input field).
		partial, _ := delta["partial_json"].(string)
		if partial != "" {
			if tb, ok := b.activeToolBlocks[index]; ok {
				tb.inputBuf.WriteString(partial)
				b.activeToolBlocks[index] = tb
			}
		}
	}
}

// handleContentBlockStop transitions tool call parts to completed status.
// This fixes the "spinner stuck" bug where tool calls stayed in PartRunning
// indefinitely because the tool_result arrives as a separate message cycle.
func (b *ClaudeCodeBackend) handleContentBlockStop(event map[string]any) {
	index := intFromAny(event["index"])

	// Only tool_use blocks are tracked in activeToolBlocks. If the index is
	// present, emit a PartCompleted update so the TUI replaces the spinner with ✓.
	tb, ok := b.activeToolBlocks[index]
	if !ok {
		return
	}
	delete(b.activeToolBlocks, index)

	// Parse accumulated input JSON into a map for the Part.Input field.
	var inputMap map[string]any
	if raw := tb.inputBuf.String(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &inputMap)
	}

	b.emit(Event{
		Type:      EventPartUpdate,
		Timestamp: time.Now(),
		Data: PartUpdateData{
			Part: Part{
				ID:     tb.partID,
				Type:   PartToolCall,
				Tool:   tb.tool,
				Status: PartCompleted,
				Input:  inputMap,
			},
		},
	})
}

// --- Type mapping helpers ---

// sessionMessageToData converts an SDK SessionMessage (parsed from the on-disk
// JSONL transcript) into a clank MessageData. Returns ok=false for messages
// that should be skipped (meta/system/unknown types, no content).
//
// Part IDs are scoped to mirror what the streaming handlers emit so that a
// future TUI dedup pass between live deltas and reloaded history can match
// them up:
//   - For tool_use / tool_result blocks the ID is the tool_use_id (same as
//     handleContentBlockStart and handleContentBlockStop).
//   - For text / thinking blocks the ID is "{apiMsgID}-{blockIdx}", where
//     apiMsgID is the Anthropic API message ID stored under msg.message.id
//     (matches blockID()). Falls back to the JSONL-level UUID when absent.
func sessionMessageToData(m claudecode.SessionMessage) (MessageData, bool) {
	if m.IsMeta {
		return MessageData{}, false
	}

	var role string
	switch m.Type {
	case "user":
		role = "user"
	case "assistant":
		role = "assistant"
	default:
		return MessageData{}, false
	}

	// Anthropic API message ID lives inside the nested "message" object;
	// fall back to the transcript-level UUID when missing (e.g. for user
	// messages which have no API id).
	msgID, _ := m.RawMessage["id"].(string)
	if msgID == "" {
		msgID = m.UUID
	}

	md := MessageData{
		ID:   msgID,
		Role: role,
	}

	if model, ok := m.RawMessage["model"].(string); ok {
		md.ModelID = model
	}

	if m.Content == nil {
		return md, true
	}

	switch m.Content.Kind {
	case claudecode.SessionContentTypeString:
		md.Content = m.Content.String
	case claudecode.SessionContentTypeBlocks:
		for i, block := range m.Content.Blocks {
			if p, ok := sessionBlockToPart(block, msgID, i); ok {
				md.Parts = append(md.Parts, p)
			}
		}
	}
	return md, true
}

// sessionBlockToPart converts an SDK session ContentBlock to a clank Part.
// The msgID/index pair scopes text/thinking IDs to match the streaming path.
func sessionBlockToPart(block claudecode.SessionContentBlock, msgID string, index int) (Part, bool) {
	switch block.Type {
	case claudecode.SessionBlockTypeText:
		return Part{
			ID:   fmt.Sprintf("%s-%d", msgID, index),
			Type: PartText,
			Text: block.Text,
		}, true
	case claudecode.SessionBlockTypeThinking:
		return Part{
			ID:   fmt.Sprintf("%s-%d", msgID, index),
			Type: PartThinking,
			Text: block.Thinking,
		}, true
	case claudecode.SessionBlockTypeToolUse, claudecode.SessionBlockTypeServerToolUse:
		return Part{
			ID:     block.ID,
			Type:   PartToolCall,
			Tool:   block.Name,
			Status: PartCompleted,
			Input:  block.Input,
		}, true
	case claudecode.SessionBlockTypeToolResult:
		status := PartCompleted
		if block.IsError != nil && *block.IsError {
			status = PartFailed
		}
		return Part{
			ID:     block.ToolUseID,
			Type:   PartToolResult,
			Status: status,
			Output: toolResultOutput(block.Content),
		}, true
	default:
		return Part{}, false
	}
}

// toolResultOutput extracts the human-readable text from a tool_result's
// content field. The SDK leaves it as `any` because the JSONL format admits
// either a string or an array of nested blocks (typically text blocks).
func toolResultOutput(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == claudecode.SessionBlockTypeText {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// intFromAny extracts an int from a JSON-decoded value (usually float64).
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
