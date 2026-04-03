// Package agent defines the interface and types for coding agent backends.
//
// Each backend (OpenCode, Claude Code) implements the Backend interface,
// allowing the daemon to manage multiple agent types uniformly.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// BackendType identifies which coding agent backend is being used.
type BackendType string

const (
	BackendOpenCode   BackendType = "opencode"
	BackendClaudeCode BackendType = "claude-code"
)

// SessionStatus represents the current state of an agent session.
type SessionStatus string

const (
	StatusStarting SessionStatus = "starting" // Process is launching
	StatusBusy     SessionStatus = "busy"     // Agent is actively working
	StatusIdle     SessionStatus = "idle"     // Agent finished, awaiting input
	StatusError    SessionStatus = "error"    // Agent encountered an error
	StatusDead     SessionStatus = "dead"     // Process exited
)

// SessionVisibility controls whether a session appears in the default inbox view.
// It is orthogonal to SessionStatus — a session can be idle at the system level
// but marked "done" by the user.
type SessionVisibility string

const (
	VisibilityVisible  SessionVisibility = ""         // Default: shown in inbox
	VisibilityDone     SessionVisibility = "done"     // User marked as completed
	VisibilityArchived SessionVisibility = "archived" // User archived (won't do / irrelevant)
)

// EventType classifies daemon events.
type EventType string

const (
	EventStatusChange  EventType = "status"       // Session status changed
	EventMessage       EventType = "message"      // New message (user or assistant)
	EventPartUpdate    EventType = "part"         // Part updated (tool call progress, text delta)
	EventPermission    EventType = "permission"   // Agent requests permission for a tool
	EventError         EventType = "error"        // Error occurred
	EventTitleChange   EventType = "title"        // Session title updated
	EventRevertChange  EventType = "revert"       // Session revert state changed
	EventReconnecting  EventType = "reconnecting" // Backend is reconnecting to server
	EventReconnected   EventType = "reconnected"  // Backend successfully reconnected
	EventSessionCreate EventType = "session.create"
	EventSessionDelete EventType = "session.delete"

	// Voice events — emitted by the voice agent running on the daemon.
	EventVoiceTranscript EventType = "voice.transcript" // Model's spoken response as text
	EventVoiceStatus     EventType = "voice.status"     // Voice state changes (listening, thinking, speaking, idle)
	EventVoiceToolCall   EventType = "voice.tool_call"  // Voice agent called a tool
)

// Event is the unified event type emitted by all backends and forwarded
// through the daemon to connected TUI clients.
type Event struct {
	Type      EventType   `json:"type"`
	SessionID string      `json:"session_id"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// UnmarshalJSON implements custom JSON unmarshalling for Event.
// It examines the "type" field to deserialize "data" into the correct concrete
// Go type instead of the default map[string]interface{}.
func (e *Event) UnmarshalJSON(b []byte) error {
	// First, decode into a raw structure to inspect the type field.
	var raw struct {
		Type      EventType       `json:"type"`
		SessionID string          `json:"session_id"`
		Timestamp time.Time       `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}

	e.Type = raw.Type
	e.SessionID = raw.SessionID
	e.Timestamp = raw.Timestamp

	// If no data payload, leave Data as nil.
	if len(raw.Data) == 0 || string(raw.Data) == "null" {
		return nil
	}

	// Deserialize Data into the correct concrete type based on event type.
	switch raw.Type {
	case EventStatusChange:
		var d StatusChangeData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal StatusChangeData: %w", err)
		}
		e.Data = d
	case EventMessage:
		var d MessageData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal MessageData: %w", err)
		}
		e.Data = d
	case EventPartUpdate:
		var d PartUpdateData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal PartUpdateData: %w", err)
		}
		e.Data = d
	case EventPermission:
		var d PermissionData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal PermissionData: %w", err)
		}
		e.Data = d
	case EventError:
		var d ErrorData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal ErrorData: %w", err)
		}
		e.Data = d
	case EventTitleChange:
		var d TitleChangeData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal TitleChangeData: %w", err)
		}
		e.Data = d
	case EventRevertChange:
		var d RevertChangeData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal RevertChangeData: %w", err)
		}
		e.Data = d
	case EventReconnecting:
		var d ReconnectingData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal ReconnectingData: %w", err)
		}
		e.Data = d
	case EventReconnected:
		var d ReconnectedData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal ReconnectedData: %w", err)
		}
		e.Data = d
	case EventVoiceTranscript:
		var d VoiceTranscriptData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal VoiceTranscriptData: %w", err)
		}
		e.Data = d
	case EventVoiceStatus:
		var d VoiceStatusData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal VoiceStatusData: %w", err)
		}
		e.Data = d
	case EventVoiceToolCall:
		var d VoiceToolCallData
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal VoiceToolCallData: %w", err)
		}
		e.Data = d
	default:
		// For unknown event types (session.create, session.delete, future types),
		// fall back to generic interface{}.
		var d interface{}
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshal event data: %w", err)
		}
		e.Data = d
	}

	return nil
}

// StatusChangeData is the payload for EventStatusChange.
type StatusChangeData struct {
	OldStatus SessionStatus `json:"old_status"`
	NewStatus SessionStatus `json:"new_status"`
}

// MessageData is the payload for EventMessage.
type MessageData struct {
	ID      string `json:"id,omitempty"` // Backend-assigned message ID (e.g. OpenCode message ID)
	Role    string `json:"role"`         // "user" or "assistant"
	Content string `json:"content"`
	Parts   []Part `json:"parts,omitempty"`
}

// Part represents a piece of an assistant message (text block, tool call, etc.).
type Part struct {
	ID     string         `json:"id"`
	Type   PartType       `json:"type"`
	Text   string         `json:"text,omitempty"`
	Tool   string         `json:"tool,omitempty"` // Tool name if tool call/result
	Status PartStatus     `json:"status,omitempty"`
	Input  map[string]any `json:"input,omitempty"`  // Tool call arguments (e.g. filePath, command)
	Output string         `json:"output,omitempty"` // Tool result text
}

// PartType classifies the content of a Part.
type PartType string

const (
	PartText       PartType = "text"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	PartThinking   PartType = "thinking"
)

// PartStatus tracks the lifecycle of a tool call.
type PartStatus string

const (
	PartPending   PartStatus = "pending"
	PartRunning   PartStatus = "running"
	PartCompleted PartStatus = "completed"
	PartFailed    PartStatus = "error"
)

// PartUpdateData is the payload for EventPartUpdate.
type PartUpdateData struct {
	MessageID string `json:"message_id,omitempty"`
	Part      Part   `json:"part"`
	// IsDelta indicates this is an incremental text chunk (append to existing
	// content). When false, Part.Text is the authoritative full snapshot and
	// should replace whatever text the TUI has accumulated for this part.
	IsDelta bool `json:"is_delta,omitempty"`
}

// PermissionData is the payload for EventPermission.
type PermissionData struct {
	RequestID   string `json:"request_id"`
	Tool        string `json:"tool"`
	Description string `json:"description"`
}

// ErrorData is the payload for EventError.
type ErrorData struct {
	Message string `json:"message"`
}

// TitleChangeData is the payload for EventTitleChange.
type TitleChangeData struct {
	Title string `json:"title"`
}

// RevertChangeData is the payload for EventRevertChange.
type RevertChangeData struct {
	MessageID string `json:"message_id"` // The message ID from which onward is reverted; empty means unrevert
}

// ReconnectingData is the payload for EventReconnecting.
type ReconnectingData struct {
	Attempt int           `json:"attempt"` // Current retry attempt (1-based)
	Delay   time.Duration `json:"delay"`   // How long until the next retry
	Error   string        `json:"error"`   // The error that triggered the reconnect
	GaveUp  bool          `json:"gave_up"` // True if this is the final failure (no more retries)
}

// ReconnectedData is the payload for EventReconnected.
type ReconnectedData struct {
	Attempts   int  `json:"attempts"`    // How many attempts it took
	URLChanged bool `json:"url_changed"` // Whether the server URL changed (new port)
}

// VoiceTranscriptData is the payload for EventVoiceTranscript.
type VoiceTranscriptData struct {
	Text string `json:"text"`           // Incremental or final transcript text
	Done bool   `json:"done,omitempty"` // True when transcript is final
}

// VoiceStatus represents the voice agent's current state.
type VoiceStatus string

const (
	VoiceStatusIdle      VoiceStatus = "idle"      // No voice session active
	VoiceStatusListening VoiceStatus = "listening" // Mic is live, user is speaking
	VoiceStatusThinking  VoiceStatus = "thinking"  // Audio committed, waiting for model
	VoiceStatusSpeaking  VoiceStatus = "speaking"  // Model is producing audio response
)

// VoiceStatusData is the payload for EventVoiceStatus.
type VoiceStatusData struct {
	Status VoiceStatus `json:"status"`
}

// VoiceToolCallData is the payload for EventVoiceToolCall.
type VoiceToolCallData struct {
	Name   string `json:"name"`             // Tool name (e.g. "list_sessions")
	Args   string `json:"args,omitempty"`   // JSON-encoded arguments
	Result string `json:"result,omitempty"` // Tool result (empty if still running)
}

// AgentInfo is a lightweight summary of an OpenCode agent, used by the TUI
// to display and cycle through available agents. We define our own struct
// rather than using the SDK's Agent type because the SDK is missing the
// "hidden" field that OpenCode returns in GET /agent.
type AgentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Mode        string `json:"mode"`   // "primary", "subagent", or "all"
	Hidden      bool   `json:"hidden"` // Internal agents (compaction, title, summary)
}

// StartRequest contains the parameters needed to start a new agent session.
type StartRequest struct {
	Backend    BackendType `json:"backend"`
	ProjectDir string      `json:"project_dir"`
	Prompt     string      `json:"prompt"`
	SessionID  string      `json:"session_id,omitempty"` // Empty = new session, set = resume
	TicketID   string      `json:"ticket_id,omitempty"`  // Optional backlog ticket link
	Agent      string      `json:"agent,omitempty"`      // OpenCode agent name (e.g. "build", "plan")
}

// Validate checks that required fields are set.
func (r StartRequest) Validate() error {
	if r.Backend == "" {
		return fmt.Errorf("backend is required")
	}
	if r.Backend != BackendOpenCode && r.Backend != BackendClaudeCode {
		return fmt.Errorf("unknown backend: %s", r.Backend)
	}
	if r.ProjectDir == "" {
		return fmt.Errorf("project_dir is required")
	}
	if r.Prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	return nil
}

// SessionInfo is a snapshot of a managed session, returned by the daemon API.
type SessionInfo struct {
	ID              string            `json:"id"`
	ExternalID      string            `json:"external_id,omitempty"` // Backend's native session ID (e.g. OpenCode session ID)
	Backend         BackendType       `json:"backend"`
	Status          SessionStatus     `json:"status"`
	Visibility      SessionVisibility `json:"visibility,omitempty"` // User-set: "", "done", or "archived"
	FollowUp        bool              `json:"follow_up,omitempty"`  // User-set flag to mark session for follow-up
	ProjectDir      string            `json:"project_dir"`
	ProjectName     string            `json:"project_name"`
	Prompt          string            `json:"prompt"`
	Title           string            `json:"title,omitempty"` // AI-generated session title from OpenCode
	TicketID        string            `json:"ticket_id,omitempty"`
	Agent           string            `json:"agent,omitempty"`             // Current OpenCode agent (e.g. "build", "plan")
	Draft           string            `json:"draft,omitempty"`             // Unsent follow-up text the user was composing
	RevertMessageID string            `json:"revert_message_id,omitempty"` // When set, messages from this ID onward are reverted (hidden)
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	LastReadAt      time.Time         `json:"last_read_at,omitempty"`
}

// ProjectInfo is a lightweight project summary from the OpenCode API.
type ProjectInfo struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

// SessionSnapshot is a lightweight session summary from the OpenCode API,
// used during discovery to populate the daemon's session list.
type SessionSnapshot struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Directory       string    `json:"directory"`
	RevertMessageID string    `json:"revert_message_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Unread returns true if the session has activity the user hasn't seen.
func (s SessionInfo) Unread() bool {
	return s.LastReadAt.IsZero() || s.UpdatedAt.After(s.LastReadAt)
}

// Hidden returns true if the session should not appear in the default inbox view.
func (s SessionInfo) Hidden() bool {
	return s.Visibility == VisibilityDone || s.Visibility == VisibilityArchived
}

// SendMessageOpts contains options for sending a follow-up message.
type SendMessageOpts struct {
	Text  string `json:"text"`
	Agent string `json:"agent,omitempty"` // OpenCode agent name; empty = use session default
}

// SessionBackend is the interface that each agent backend must implement.
// The daemon creates one SessionBackend instance per session.
type SessionBackend interface {
	// Start launches the agent with the given request.
	// It blocks for the duration of the LLM turn; events stream via Events().
	Start(ctx context.Context, req StartRequest) error

	// Watch starts listening for events on this session without sending a
	// prompt. Use this to observe a session that may already be active
	// (e.g. a discovered/historical session). Backends that don't support
	// passive observation (like Claude CLI) should return nil (no-op).
	// The Events channel must produce events after Watch returns.
	Watch(ctx context.Context) error

	// SendMessage sends a follow-up message to the running agent.
	SendMessage(ctx context.Context, opts SendMessageOpts) error

	// Abort interrupts the currently running agent task.
	Abort(ctx context.Context) error

	// Stop gracefully shuts down the backend and cleans up resources.
	Stop() error

	// Events returns a channel that receives all events from this backend.
	// The channel is closed when the backend stops.
	Events() <-chan Event

	// Status returns the current session status.
	Status() SessionStatus

	// SessionID returns the agent-assigned session ID (may differ from the daemon's ID).
	SessionID() string

	// Messages returns the full message history for this session.
	// Each MessageData includes role, content, and parts.
	// Returns nil, nil if the backend does not support history retrieval.
	Messages(ctx context.Context) ([]MessageData, error)

	// Revert reverts the session to the specified message, removing all
	// subsequent messages. Returns an error if the backend does not support
	// reverting (e.g. Claude Code).
	Revert(ctx context.Context, messageID string) error
}

// BackendManager creates and manages SessionBackend instances for a specific
// backend type. Each implementation handles its own resource sharing (e.g.,
// OpenCode shares one server per project directory, Claude manages
// subprocesses independently).
type BackendManager interface {
	// Init performs eager initialization such as starting servers for known
	// project directories. Called once by the daemon on startup before any
	// other method. Long-running work (like reconciler loops) should be
	// launched as goroutines that respect ctx cancellation.
	// knownDirs returns project directories previously seen for this backend.
	Init(ctx context.Context, knownDirs func() ([]string, error)) error

	// CreateBackend creates a new SessionBackend for the given request.
	// The backend is not started — call Start() or Watch() on it.
	CreateBackend(req StartRequest) (SessionBackend, error)

	// Shutdown cleans up all managed resources (servers, connections, etc.).
	Shutdown()
}

// AgentLister is an optional interface that BackendManagers can implement
// to expose available agents for a project.
type AgentLister interface {
	ListAgents(ctx context.Context, projectDir string) ([]AgentInfo, error)
}

// SessionDiscoverer is an optional interface that BackendManagers can
// implement to discover historical sessions from the underlying backend.
type SessionDiscoverer interface {
	DiscoverSessions(ctx context.Context, seedDir string) ([]SessionSnapshot, error)
}

// ServerInfo is a snapshot of a running backend server process (e.g. an
// `opencode serve` instance). Used by debugging/status commands.
type ServerInfo struct {
	URL        string    `json:"url"`
	ProjectDir string    `json:"project_dir"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
}

// ServerLister is an optional interface that BackendManagers can implement
// to expose information about running backend server processes.
type ServerLister interface {
	ListServers() []ServerInfo
}
