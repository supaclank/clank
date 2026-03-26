package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ClaudeCodeBackend manages a single Claude Code session by spawning
// `claude -p` as a subprocess and parsing its streaming JSON output.
//
// Architecture:
//   - Each backend instance corresponds to one session.
//   - The subprocess is started in the project directory.
//   - Events are parsed from stdout (--output-format stream-json).
//   - Resume uses --resume <session_id>.
//   - Abort sends SIGINT to the process.
type ClaudeCodeBackend struct {
	mu        sync.Mutex
	status    SessionStatus
	sessionID string // Claude's session ID (parsed from "system" init event)
	events    chan Event
	ctx       context.Context
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stdin     io.WriteCloser

	// CmdFactory allows tests to inject a custom command builder.
	// If nil, uses the default `claude` CLI.
	CmdFactory func(ctx context.Context, args []string, dir string) *exec.Cmd
}

// NewClaudeCodeBackend creates a new Claude Code backend.
func NewClaudeCodeBackend() *ClaudeCodeBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &ClaudeCodeBackend{
		status: StatusStarting,
		events: make(chan Event, 128),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (b *ClaudeCodeBackend) Start(ctx context.Context, req StartRequest) error {
	args := []string{
		"-p", req.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}

	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
		b.sessionID = req.SessionID
	}

	var cmd *exec.Cmd
	if b.CmdFactory != nil {
		cmd = b.CmdFactory(b.ctx, args, req.ProjectDir)
	} else {
		cmd = exec.CommandContext(b.ctx, "claude", args...)
		cmd.Dir = req.ProjectDir
		// Own process group so it can be signalled independently.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}

	b.mu.Lock()
	b.cmd = cmd
	b.stdin = stdin
	b.mu.Unlock()

	b.setStatus(StatusBusy)

	// Parse stdout in background.
	go b.parseOutput(stdout, stderr)

	return nil
}

func (b *ClaudeCodeBackend) SendMessage(ctx context.Context, opts SendMessageOpts) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cmd == nil || b.cmd.Process == nil {
		return fmt.Errorf("session not started")
	}

	// For follow-up messages, we need to start a new process with --resume.
	// Claude Code doesn't support writing to stdin of an existing -p session.
	// The caller (daemon) should create a new backend with SessionID set.
	return fmt.Errorf("claude code follow-up requires a new process with --resume; use Start with SessionID set")
}

func (b *ClaudeCodeBackend) Abort(ctx context.Context) error {
	b.mu.Lock()
	cmd := b.cmd
	b.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("session not started")
	}

	// Send SIGINT to the process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		// Fallback to sending directly to the process.
		return cmd.Process.Signal(os.Interrupt)
	}
	return nil
}

func (b *ClaudeCodeBackend) Stop() error {
	b.cancel()

	b.mu.Lock()
	cmd := b.cmd
	b.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		// Give it a moment to exit gracefully.
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
		}
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

// Messages is not yet implemented for Claude Code.
// Claude Code supports --resume with --replay-user-messages for streaming
// history, and the --ide flag may provide structured output. The long-term
// plan is to store messages in our own DB from the stream-json output.
func (b *ClaudeCodeBackend) Messages(ctx context.Context) ([]MessageData, error) {
	return nil, nil
}

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
	select {
	case b.events <- evt:
	default:
		// Drop if full.
	}
}

func (b *ClaudeCodeBackend) emitError(msg string) {
	b.emit(Event{
		Type:      EventError,
		Timestamp: time.Now(),
		Data:      ErrorData{Message: msg},
	})
}

// parseOutput reads stdout line-by-line, parsing each JSON object.
// Each line from `claude -p --output-format stream-json` is a JSON object
// with a `type` field.
func (b *ClaudeCodeBackend) parseOutput(stdout, stderr io.Reader) {
	defer close(b.events)

	// Drain stderr in a separate goroutine.
	go func() {
		io.Copy(io.Discard, stderr)
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg claudeMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // Skip non-JSON lines.
		}

		b.handleClaudeMessage(msg, line)
	}

	// Process exited — wait for it and update status.
	b.mu.Lock()
	cmd := b.cmd
	b.mu.Unlock()

	if cmd != nil {
		cmd.Wait()
	}

	if b.Status() == StatusBusy || b.Status() == StatusStarting {
		b.setStatus(StatusDead)
	}
}

// claudeMessage is the raw shape of each streaming JSON line.
type claudeMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`

	// For type=system, subtype=init
	SessionID string `json:"session_id,omitempty"`

	// For type=assistant
	Message *claudeAssistantMessage `json:"message,omitempty"`

	// For type=result
	Result   string  `json:"result,omitempty"`
	CostUSD  float64 `json:"total_cost_usd,omitempty"`
	Duration float64 `json:"duration_ms,omitempty"`
	IsError  bool    `json:"is_error,omitempty"`

	// For type=content_block_start, content_block_delta, content_block_stop
	Index        int                 `json:"index,omitempty"`
	ContentBlock *claudeContentBlock `json:"content_block,omitempty"`
	Delta        *claudeContentBlock `json:"delta,omitempty"`
}

type claudeAssistantMessage struct {
	ID      string               `json:"id,omitempty"`
	Role    string               `json:"role,omitempty"`
	Content []claudeContentBlock `json:"content,omitempty"`
}

type claudeContentBlock struct {
	Type string `json:"type,omitempty"` // "text", "tool_use", "tool_result", "thinking"
	Text string `json:"text,omitempty"`

	// Tool use fields
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// Tool result fields
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

func (b *ClaudeCodeBackend) handleClaudeMessage(msg claudeMessage, raw []byte) {
	switch msg.Type {
	case "system":
		if msg.Subtype == "init" && msg.SessionID != "" {
			b.mu.Lock()
			b.sessionID = msg.SessionID
			b.mu.Unlock()
		}

	case "assistant":
		if msg.Message == nil {
			return
		}
		// Emit a message event.
		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data: MessageData{
				Role: msg.Message.Role,
			},
		})

		// Emit part updates for each content block.
		for _, block := range msg.Message.Content {
			b.emitContentBlock(block)
		}

	case "content_block_start":
		if msg.ContentBlock != nil {
			b.emitContentBlock(*msg.ContentBlock)
		}

	case "content_block_delta":
		if msg.Delta != nil {
			b.emitContentBlockDelta(msg.Index, *msg.Delta)
		}

	case "content_block_stop":
		// Could track completion of individual blocks if needed.

	case "result":
		if msg.IsError {
			b.setStatus(StatusError)
			b.emitError(msg.Result)
		} else {
			b.setStatus(StatusIdle)
		}

		// Emit a message event with the result.
		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data: MessageData{
				Role:    "assistant",
				Content: msg.Result,
			},
		})
	}
}

func (b *ClaudeCodeBackend) emitContentBlock(block claudeContentBlock) {
	var partType PartType
	var partStatus PartStatus
	var tool string
	var text string

	switch block.Type {
	case "text":
		partType = PartText
		text = block.Text
	case "tool_use":
		partType = PartToolCall
		tool = block.Name
		partStatus = PartRunning
		if block.Input != nil {
			text = string(block.Input)
		}
	case "tool_result":
		partType = PartToolResult
		partStatus = PartCompleted
		text = block.Content
	case "thinking":
		partType = PartThinking
		text = block.Text
	default:
		return
	}

	b.emit(Event{
		Type:      EventPartUpdate,
		Timestamp: time.Now(),
		Data: PartUpdateData{
			Part: Part{
				ID:     block.ID,
				Type:   partType,
				Text:   text,
				Tool:   tool,
				Status: partStatus,
			},
		},
	})
}

func (b *ClaudeCodeBackend) emitContentBlockDelta(index int, delta claudeContentBlock) {
	if delta.Type == "text_delta" || delta.Text != "" {
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:   fmt.Sprintf("block-%d", index),
					Type: PartText,
					Text: delta.Text,
				},
			},
		})
	}
}
