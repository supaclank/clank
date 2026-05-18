// Package agent defines the interface and types for coding agent backends.
//
// Each backend (OpenCode, Claude Code) implements the Backend interface,
// allowing the daemon to manage multiple agent types uniformly.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
	VisibilityAll      SessionVisibility = "all"      // Pseudo-value: include all visibilities
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
	Type      EventType `json:"type"`
	SessionID string    `json:"session_id"` // hub session id (set by hub relay)
	// ExternalID carries the session-backend's native session ID. TODO rename
	ExternalID string      `json:"external_id,omitempty"`
	Timestamp  time.Time   `json:"timestamp"`
	Data       interface{} `json:"data"`
}

// UnmarshalJSON implements custom JSON unmarshalling for Event.
// It examines the "type" field to deserialize "data" into the correct concrete
// Go type instead of the default map[string]interface{}.
func (e *Event) UnmarshalJSON(b []byte) error {
	// First, decode into a raw structure to inspect the type field.
	var raw struct {
		Type       EventType       `json:"type"`
		SessionID  string          `json:"session_id"`
		ExternalID string          `json:"external_id"`
		Timestamp  time.Time       `json:"timestamp"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}

	e.Type = raw.Type
	e.SessionID = raw.SessionID
	e.ExternalID = raw.ExternalID
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
	ID         string `json:"id,omitempty"` // Backend-assigned message ID (e.g. OpenCode message ID)
	Role       string `json:"role"`         // "user" or "assistant"
	Content    string `json:"content"`
	Parts      []Part `json:"parts,omitempty"`
	ModelID    string `json:"model_id,omitempty"`    // Model that produced this message (assistant only)
	ProviderID string `json:"provider_id,omitempty"` // Provider of the model (assistant only)
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

// ForkResult holds the result of a Fork operation.
type ForkResult struct {
	ID    string // External (backend) session ID
	Title string // AI-generated title carried over from the source session
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

// LaunchHostSpec asks the Hub to provision a fresh Host (sandbox) for
// this session before dispatching it. When set, the Hub consults a
// registered HostLauncher (e.g. "daytona", "local-stub") which spins
// up a Host, registers it in the catalog, and rewrites Hostname to
// the launcher-chosen name.
//
// Mutually exclusive with Hostname (Hostname is the launcher's output,
// not its input).
type LaunchHostSpec struct {
	Provider string `json:"provider"` // "daytona", "local-stub", ...
}

// StartRequest contains the parameters needed to start a new agent session.
//
// Identity is path-free post Phase 3D-2 (hub_host_refactor.md):
// (Hostname, GitRef, WorktreeBranch). The Host resolves these to a working
// directory inside CreateSession; the wire never carries filesystem paths.
//
// GitRef is the sole repo identity on the wire (§7.3 of
// hub_host_refactor_code_review.md).
type StartRequest struct {
	Backend    BackendType     `json:"backend"`
	Hostname   string          `json:"hostname,omitempty"`    // Target host; empty defaults to "local" at the hub.
	LaunchHost *LaunchHostSpec `json:"launch_host,omitempty"` // When set, Hub provisions a fresh host before dispatching.
	GitRef     GitRef          `json:"git_ref"`               // Wire-canonical repo identity; required. WorktreeBranch lives inside.
	Prompt     string          `json:"prompt"`
	SessionID  string          `json:"session_id,omitempty"` // Backend-external session ID for resume; empty = new session.
	TicketID   string          `json:"ticket_id,omitempty"`  // Optional backlog ticket link
	Agent      string          `json:"agent,omitempty"`      // OpenCode agent name (e.g. "build", "plan")
	Model      *ModelOverride  `json:"model,omitempty"`      // Per-message model override; nil = use default
}

// Validate checks that required fields are set per §7.3 of
// hub_host_refactor_code_review.md.
func (r StartRequest) Validate() error {
	if r.Backend == "" {
		return fmt.Errorf("backend is required")
	}
	if r.Backend != BackendOpenCode && r.Backend != BackendClaudeCode {
		return fmt.Errorf("unknown backend: %s", r.Backend)
	}
	if err := r.GitRef.Validate(); err != nil {
		return fmt.Errorf("git_ref: %w", err)
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
	Hostname        string            `json:"hostname,omitempty"`   // Canonical identity: host (Phase 3); "local" by default.
	GitRef          GitRef            `json:"git_ref,omitempty"`    // Canonical identity: repo (LocalPath and/or RemoteURL + WorktreeBranch).
	Prompt          string            `json:"prompt"`
	Title           string            `json:"title,omitempty"` // AI-generated session title from OpenCode
	TicketID        string            `json:"ticket_id,omitempty"`
	Agent           string            `json:"agent,omitempty"`             // Current OpenCode agent (e.g. "build", "plan")
	Draft           string            `json:"draft,omitempty"`             // Unsent follow-up text the user was composing
	RevertMessageID string            `json:"revert_message_id,omitempty"` // When set, messages from this ID onward are reverted (hidden)
	ServerURL       string            `json:"server_url,omitempty"`        // Runtime-only: backend server URL (e.g. OpenCode serve endpoint). Not persisted.
	IsRemote        bool              `json:"is_remote,omitempty"`         // Runtime-only: decoration stamped by the laptop daemon's session router when this session's worktree is owned by the active remote. Always false on direct host responses; populated by gateway routing.
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	LastReadAt      time.Time         `json:"last_read_at,omitempty"`
}

// SearchParams defines the parameters for searching sessions.
//
// Query supports pipe-separated OR groups with space-separated AND terms
// within each group. For example, "auth bug|dark mode" matches sessions
// containing ("auth" AND "bug") OR ("dark" AND "mode"). Matching is
// case-insensitive and word-boundary-aware: each term must appear at the
// start of a word (e.g. "auth" matches "authentication" but "hey" does not
// match "they").
//
// Since and Until filter on UpdatedAt. Both are optional; when omitted
// the corresponding bound is open.
type SearchParams struct {
	Query      string            `json:"query,omitempty"`      // pipe-separated OR groups
	Since      time.Time         `json:"since,omitempty"`      // only sessions updated at or after this time
	Until      time.Time         `json:"until,omitempty"`      // only sessions updated before this time
	Visibility SessionVisibility `json:"visibility,omitempty"` // "" = active only, "all" = everything, "done"/"archived" = only that
}

// ParseTimeParam parses a time string that is either a relative duration
// suffix (e.g. "7d", "24h") interpreted as "ago from now", or an RFC 3339
// timestamp. Supported relative units: h (hours), d (days).
func ParseTimeParam(s string) (time.Time, error) {
	if len(s) < 2 {
		return time.Time{}, fmt.Errorf("too short: %q", s)
	}

	unit := s[len(s)-1]
	if unit == 'h' || unit == 'd' {
		numStr := s[:len(s)-1]
		n, err := strconv.Atoi(numStr)
		if err == nil && n > 0 {
			switch unit {
			case 'h':
				return time.Now().Add(-time.Duration(n) * time.Hour), nil
			case 'd':
				return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
			}
		}
	}

	// Fall back to RFC 3339.
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected relative duration (e.g. 7d, 24h) or RFC 3339 timestamp, got %q", s)
	}
	return t, nil
}

// ProjectInfo is a lightweight project summary from the OpenCode API.
type ProjectInfo struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

// SessionSnapshot is a lightweight session summary returned by a backend
// manager's DiscoverSessions, used to populate the daemon's session list.
//
// Backend identifies which backend produced the snapshot. The hub aggregates
// snapshots from multiple backends in a single slice, so without this field
// it is impossible to attribute a snapshot to its source backend at the
// registration site. Persisting the wrong Backend on a discovered session
// causes activateBackend (after a daemon restart) to route the reopen
// through the wrong backend manager, which manifests as a permanent
// "Waiting for agent output..." hang for Claude sessions that were
// mis-tagged as opencode.
type SessionSnapshot struct {
	ID              string      `json:"id"`
	Backend         BackendType `json:"backend"`
	Title           string      `json:"title"`
	Directory       string      `json:"directory"`
	RevertMessageID string      `json:"revert_message_id,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

// Unread returns true if the session has activity the user hasn't seen.
func (s SessionInfo) Unread() bool {
	return s.LastReadAt.IsZero() || s.UpdatedAt.After(s.LastReadAt)
}

// Hidden returns true if the session should not appear in the default inbox view.
func (s SessionInfo) Hidden() bool {
	return s.Visibility == VisibilityDone || s.Visibility == VisibilityArchived
}

// ModelOverride specifies a model+provider to use for a single message,
// overriding the backend's default.
type ModelOverride struct {
	ModelID    string `json:"model_id"`
	ProviderID string `json:"provider_id"`
}

// SendMessageOpts contains options for sending a follow-up message.
type SendMessageOpts struct {
	Text  string         `json:"text"`
	Agent string         `json:"agent,omitempty"` // OpenCode agent name; empty = use session default
	Model *ModelOverride `json:"model,omitempty"` // Per-message model override; nil = use default
}

// Lifecycle: NewBackend → Open (or OpenAndSend) → Send* → Abort? → Stop
//
// Concurrency: all methods must be safe to call concurrently from
// multiple goroutines.
//
// Event timing: backends emit events asynchronously via Events(). Method
// returns describe what their *return* signals — typically "request
// dispatched" — NOT when the agent has finished work. Observe completion
// via Events() (StatusChange to Idle) and ExternalID via Event.ExternalID.
//
// Session-scoped configuration (workDir, resume external ID, host/server
// selection) is supplied to the constructor, not to these methods. The
// methods below carry only per-prompt data.
type SessionBackend interface {
	// Open establishes (or re-attaches to) the session and begins event
	// production into Events(). Idempotent — safe to call on an
	// already-open session.
	Open(ctx context.Context) error

	// Send dispatches a prompt to an Open session. Fast-fails if the
	// session is not open. Returns once the prompt is handed off to
	// the agent runtime, NOT when the LLM finishes.
	Send(ctx context.Context, opts SendMessageOpts) error

	// OpenAndSend is the new-session convenience: Open followed by Send.
	// Backends MAY fuse the two operations when their runtime supports
	// it (e.g. dispatching the prompt as part of session creation).
	OpenAndSend(ctx context.Context, opts SendMessageOpts) error

	// Abort signals the agent to interrupt the current turn. Best-effort:
	// returns once the signal has been delivered, not when the agent has
	// actually stopped. Observe StatusChange events for completion.
	Abort(ctx context.Context) error

	// Stop performs a graceful shutdown: closes the event channel,
	// terminates child processes, and releases resources. Blocks until
	// teardown completes. Safe to call multiple times.
	Stop() error

	// Events returns the event stream for this backend. The channel is
	// closed when the backend stops. All events for a session flow
	// through this channel
	Events() <-chan Event

	// Status returns the current session status snapshot. May change
	// concurrently; treat as a hint, not authoritative.
	Status() SessionStatus

	// SessionID returns the agent-assigned native session ID, or "" if
	// not yet known. Used by HTTP handlers to serialize ExternalID and
	// by discover for deduplication. Hub code MUST NOT poll this after
	// Open to persist the ID — use Event.ExternalID instead, which is
	// the single source of truth that survives daemon restarts.
	SessionID() string

	// Messages returns the on-disk transcript for this session. Reads
	// fresh from the backend's storage on each call (no in-memory
	// accumulation fallback). Returns (nil, nil) if no transcript
	// exists yet (e.g. session ID not learned, or backend doesn't
	// support history retrieval).
	Messages(ctx context.Context) ([]MessageData, error)

	// Revert removes the specified message and all subsequent messages.
	// Returns an error if the backend does not support reverting
	// (e.g. Claude Code).
	Revert(ctx context.Context, messageID string) error

	// Fork creates a new session branched from the given message.
	// Returns the new session's external ID and title. Returns an error
	// if the backend does not support forking.
	Fork(ctx context.Context, messageID string) (ForkResult, error)

	// RespondPermission replies to a pending permission prompt.
	// allow=true sends "once", allow=false sends "reject".
	// Returns an error if the backend does not support permissions.
	RespondPermission(ctx context.Context, permissionID string, allow bool) error
}

// BackendInvocation is the host-resolved, backend-only view of a session
// start. It is constructed inside host.Service.CreateSession after the
// (GitRef, WorktreeBranch) → workDir resolution; it never appears on the
// wire. See §7.4 of hub_host_refactor_code_review.md.
type BackendInvocation struct {
	// WorkDir is the resolved filesystem path where the backend should run
	// (a repo root, or a worktree path when WorktreeBranch was set).
	WorkDir string

	// ResumeExternalID is the backend's own session ID for resume; empty
	// means start a new backend session. Currently this is sourced from
	// StartRequest.SessionID (which doubles as the host-side session ID).
	// A future split may decouple the two.
	ResumeExternalID string
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

	// CreateBackend creates a new SessionBackend from a host-resolved
	// invocation. The wire StartRequest is path-free; the Host resolves
	// (RepoRef, Branch) → workDir and constructs a BackendInvocation
	// before invoking this method. See §7.4 of hub_host_refactor_code_review.md.
	// The backend is not started — call Start() or Watch() on it.
	CreateBackend(ctx context.Context, inv BackendInvocation) (SessionBackend, error)

	// Shutdown cleans up all managed resources (servers, connections, etc.).
	Shutdown()
}

// AgentLister is an optional interface that BackendManagers can implement
// to expose available agents for a project.
type AgentLister interface {
	ListAgents(ctx context.Context, projectDir string) ([]AgentInfo, error)
}

// ModelInfo is a lightweight summary of an available LLM model, used by the
// TUI to display and cycle through models.
type ModelInfo struct {
	ID           string `json:"id"`            // Model ID (e.g. "claude-opus-4-20250514")
	Name         string `json:"name"`          // Human-readable name (e.g. "Claude Opus")
	ProviderID   string `json:"provider_id"`   // Provider ID (e.g. "github-copilot")
	ProviderName string `json:"provider_name"` // Human-readable provider name
}

// ModelLister is an optional interface that BackendManagers can implement
// to expose available models for a project.
type ModelLister interface {
	ListModels(ctx context.Context, projectDir string) ([]ModelInfo, error)
}

// SessionDiscoverer is an optional interface that BackendManagers can
// implement to discover historical sessions from the underlying backend.
type SessionDiscoverer interface {
	DiscoverSessions(ctx context.Context, seedDir string) ([]SessionSnapshot, error)
}

// AllSessionDiscoverer is an optional interface for BackendManagers whose
// underlying storage allows enumerating every historical session globally
// (across all known projects) without first naming a seed directory.
//
// Used by the hub's startup-discover pass to heal mis-tagged info.Backend
// rows: after a corrupted persistence (Backend=opencode for what is really
// a Claude session), the hub does not know which project dir to query, so
// the per-seedDir DiscoverSessions path can't find it. AllSessionDiscoverer
// lets the hub enumerate every snapshot the backend knows about, regardless
// of the persisted (and potentially wrong) GitRef.LocalPath.
//
// Backends whose discovery model is per-project (e.g. opencode, which boots
// one HTTP server per project worktree) deliberately do NOT implement this.
type AllSessionDiscoverer interface {
	DiscoverAllSessions(ctx context.Context) ([]SessionSnapshot, error)
}

// ServerInfo is a snapshot of a running backend server process (e.g. an
// `opencode serve` instance). Used by debugging/status commands.
type ServerInfo struct {
	URL        string    `json:"url"`
	ProjectDir string    `json:"project_dir"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
}
