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
//
// Future: When the SDK adds list_sessions() and get_session_messages()
// (see https://github.com/severity1/claude-agent-sdk-go/issues/107),
// Messages() can retrieve full history from Claude's native storage
// instead of relying on in-memory accumulation.
type ClaudeCodeBackend struct {
	mu         sync.Mutex
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

	// messages accumulates MessageData from the stream for Messages() retrieval.
	// Lost on daemon restart; future: persist to SQLite or use SDK session history.
	messages []MessageData

	// ClientFactory builds a claudecode.Client for a given set of options.
	// Tests inject a factory that returns a client backed by a mock transport.
	// If nil, the default claudecode.NewClient is used.
	ClientFactory func(opts ...claudecode.Option) claudecode.Client
}

// NewClaudeCodeBackend creates a new Claude Code backend. workDir is
// the host-resolved working directory (worktree or repo root) the
// claude CLI will be launched in.
func NewClaudeCodeBackend(workDir string) *ClaudeCodeBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &ClaudeCodeBackend{
		status:           StatusStarting,
		projectDir:       workDir,
		events:           make(chan Event, 128),
		activeToolBlocks: make(map[int]activeToolBlock),
		ctx:              ctx,
		cancel:           cancel,
	}
}

func (b *ClaudeCodeBackend) Start(ctx context.Context, req StartRequest) error {
	b.mu.Lock()
	workDir := b.projectDir
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

	if req.SessionID != "" {
		opts = append(opts, claudecode.WithResume(req.SessionID))
		b.mu.Lock()
		b.sessionID = req.SessionID
		b.mu.Unlock()
	}

	// Build the client — use the test factory if provided.
	var client claudecode.Client
	b.mu.Lock()
	factory := b.ClientFactory
	b.mu.Unlock()

	if factory != nil {
		client = factory(opts...)
	} else {
		client = claudecode.NewClient(opts...)
	}

	b.mu.Lock()
	b.client = client
	b.mu.Unlock()

	if err := b.client.Connect(b.ctx); err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("connect to claude CLI: %w", err)
	}

	b.setStatus(StatusBusy)

	// Start receiving messages from the SDK in the background.
	go b.receiveLoop()

	// Send the initial prompt.
	if err := b.client.Query(b.ctx, req.Prompt); err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("send initial prompt: %w", err)
	}

	return nil
}

func (b *ClaudeCodeBackend) SendMessage(ctx context.Context, opts SendMessageOpts) error {
	b.mu.Lock()
	client := b.client
	b.mu.Unlock()

	if client == nil {
		return fmt.Errorf("session not started: client not connected")
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

	b.mu.Lock()
	b.messages = append(b.messages, MessageData{
		Role:    "user",
		Content: opts.Text,
	})
	b.mu.Unlock()

	b.setStatus(StatusBusy)

	if err := client.Query(b.ctx, opts.Text); err != nil {
		return fmt.Errorf("send follow-up: %w", err)
	}

	return nil
}

// Watch is a no-op for Claude Code. The CLI doesn't support passive
// observation — events only flow while the subprocess is running.
func (b *ClaudeCodeBackend) Watch(ctx context.Context) error {
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

// Messages returns the conversation history accumulated during this session.
// Each assistant turn and user follow-up is recorded as the stream is processed.
//
// Future: When the SDK adds list_sessions() / get_session_messages()
// (https://github.com/severity1/claude-agent-sdk-go/issues/107),
// this can retrieve full history from Claude's native session storage
// instead of relying on in-memory accumulation.
func (b *ClaudeCodeBackend) Messages(ctx context.Context) ([]MessageData, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.messages == nil {
		return nil, nil
	}
	// Return a copy to avoid races.
	msgs := make([]MessageData, len(b.messages))
	copy(msgs, b.messages)
	return msgs, nil
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
	if m.Subtype == "init" {
		if sid, ok := m.Data["session_id"].(string); ok && sid != "" {
			b.mu.Lock()
			b.sessionID = sid
			b.mu.Unlock()
		}
	}
}

func (b *ClaudeCodeBackend) handleAssistantMessage(m *claudecode.AssistantMessage) {
	// Build parts from the SDK's typed content blocks for Messages() accumulation.
	// IDs use currentMsgID matching the streaming path so that seenParts dedup
	// (populated from Messages() between turns) correctly matches streaming IDs.
	var parts []Part
	for i, block := range m.Content {
		if p, ok := contentBlockToPart(block, b.currentMsgID, i); ok {
			parts = append(parts, p)
		}
	}

	// Accumulate for Messages().
	md := MessageData{
		Role:  "assistant",
		Parts: parts,
	}
	b.mu.Lock()
	b.messages = append(b.messages, md)
	b.mu.Unlock()

	// Emit a content-less shell — matching the OpenCode pattern.
	// The TUI ignores EventMessage content after history loads, and new
	// content arrives exclusively via EventPartUpdate from streaming deltas
	// (handleContentBlockStart/Delta). Emitting parts here would duplicate
	// what the streaming path already delivered.
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

// contentBlockToPart maps an SDK ContentBlock to a clank Part.
// The msgID and index parameters produce a message-scoped ID ("{msgID}-{index}")
// matching the IDs emitted by the streaming handlers.
func contentBlockToPart(block claudecode.ContentBlock, msgID string, index int) (Part, bool) {
	id := fmt.Sprintf("%s-%d", msgID, index)
	switch b := block.(type) {
	case *claudecode.TextBlock:
		return Part{
			ID:   id,
			Type: PartText,
			Text: b.Text,
		}, true
	case *claudecode.ToolUseBlock:
		return Part{
			ID:     b.ToolUseID,
			Type:   PartToolCall,
			Tool:   b.Name,
			Status: PartCompleted,
			Input:  b.Input,
		}, true
	case *claudecode.ToolResultBlock:
		var output string
		if s, ok := b.Content.(string); ok {
			output = s
		}
		return Part{
			ID:     b.ToolUseID,
			Type:   PartToolResult,
			Status: PartCompleted,
			Output: output,
		}, true
	case *claudecode.ThinkingBlock:
		return Part{
			ID:   id,
			Type: PartThinking,
			Text: b.Thinking,
		}, true
	default:
		return Part{}, false
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
