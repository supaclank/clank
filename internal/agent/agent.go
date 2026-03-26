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

// EventType classifies daemon events.
type EventType string

const (
	EventStatusChange  EventType = "status"     // Session status changed
	EventMessage       EventType = "message"    // New message (user or assistant)
	EventPartUpdate    EventType = "part"       // Part updated (tool call progress, text delta)
	EventPermission    EventType = "permission" // Agent requests permission for a tool
	EventError         EventType = "error"      // Error occurred
	EventSessionCreate EventType = "session.create"
	EventSessionDelete EventType = "session.delete"
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
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
	Parts   []Part `json:"parts,omitempty"`
}

// Part represents a piece of an assistant message (text block, tool call, etc.).
type Part struct {
	ID     string     `json:"id"`
	Type   PartType   `json:"type"`
	Text   string     `json:"text,omitempty"`
	Tool   string     `json:"tool,omitempty"` // Tool name if tool call/result
	Status PartStatus `json:"status,omitempty"`
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

// StartRequest contains the parameters needed to start a new agent session.
type StartRequest struct {
	Backend    BackendType `json:"backend"`
	ProjectDir string      `json:"project_dir"`
	Prompt     string      `json:"prompt"`
	SessionID  string      `json:"session_id,omitempty"` // Empty = new session, set = resume
	TicketID   string      `json:"ticket_id,omitempty"`  // Optional backlog ticket link
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
	ID          string        `json:"id"`
	Backend     BackendType   `json:"backend"`
	Status      SessionStatus `json:"status"`
	ProjectDir  string        `json:"project_dir"`
	ProjectName string        `json:"project_name"`
	Prompt      string        `json:"prompt"`
	TicketID    string        `json:"ticket_id,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	LastReadAt  time.Time     `json:"last_read_at,omitempty"`
}

// Unread returns true if the session has activity the user hasn't seen.
func (s SessionInfo) Unread() bool {
	return s.LastReadAt.IsZero() || s.UpdatedAt.After(s.LastReadAt)
}

// Backend is the interface that each agent backend must implement.
// The daemon creates one Backend instance per session.
type Backend interface {
	// Start launches the agent with the given request.
	// It should return quickly; the agent runs asynchronously.
	// Events are delivered via the Events channel.
	Start(ctx context.Context, req StartRequest) error

	// SendMessage sends a follow-up message to the running agent.
	SendMessage(ctx context.Context, text string) error

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
}
