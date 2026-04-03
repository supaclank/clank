// Package voice provides a voice conversation agent that can manage
// coding agent sessions on the Clank daemon. It wraps the mindmouth
// library for OpenAI Realtime API integration and exposes daemon
// operations as voice-callable tools.
package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/mindmouth/tools"
)

// ToolProvider abstracts daemon operations that voice tools need.
// The daemon implements this directly (in-process). If the voice agent
// is ever extracted into a separate process, swap in an implementation
// that shells out to the clank CLI or calls the HTTP API.
type ToolProvider interface {
	ListSessions(ctx context.Context) ([]agent.SessionInfo, error)
	GetSession(ctx context.Context, id string) (*agent.SessionInfo, error)
	GetSessionMessages(ctx context.Context, sessionID string) ([]agent.MessageData, error)
	SendMessage(ctx context.Context, sessionID string, text string) error
	CreateSession(ctx context.Context, req agent.StartRequest) (*agent.SessionInfo, error)
	AbortSession(ctx context.Context, sessionID string) error
}

// RegisterTools adds clankd tools to the given registry. Each tool
// delegates to the ToolProvider for execution.
func RegisterTools(reg *tools.Registry, tp ToolProvider) {
	reg.Register(listSessionsTool(tp))
	reg.Register(getSessionTool(tp))
	reg.Register(getMessagesTool(tp))
	reg.Register(sendMessageTool(tp))
	reg.Register(createSessionTool(tp))
	reg.Register(abortSessionTool(tp))
}

func listSessionsTool(tp ToolProvider) *tools.Tool {
	return &tools.Tool{
		Name:        "list_sessions",
		Description: "List all coding agent sessions with their status, title, project, and whether they need attention.",
		Schema: tools.InputSchema{
			Properties: map[string]any{},
		},
		Effect: tools.ReadOnly,
		Fn: func(input json.RawMessage) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sessions, err := tp.ListSessions(ctx)
			if err != nil {
				return "", fmt.Errorf("list sessions: %w", err)
			}
			if len(sessions) == 0 {
				return "No sessions found.", nil
			}
			var b strings.Builder
			for _, s := range sessions {
				unread := ""
				if s.Unread() {
					unread = " [UNREAD]"
				}
				title := s.Title
				if title == "" {
					title = truncate(s.Prompt, 60)
				}
				fmt.Fprintf(&b, "- %s | %s | %s | %s | %s%s\n",
					s.ID[:8], s.Status, s.Backend, s.ProjectName, title, unread)
			}
			return b.String(), nil
		},
	}
}

func getSessionTool(tp ToolProvider) *tools.Tool {
	return &tools.Tool{
		Name:        "get_session",
		Description: "Get detailed information about a specific session by its ID (or ID prefix).",
		Schema: tools.InputSchema{
			Properties: map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "The session ID or prefix (at least 4 characters).",
				},
			},
			Required: []string{"session_id"},
		},
		Effect: tools.ReadOnly,
		Fn: func(input json.RawMessage) (string, error) {
			var args struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Resolve prefix to full ID.
			id, err := resolveSessionID(ctx, tp, args.SessionID)
			if err != nil {
				return "", err
			}

			info, err := tp.GetSession(ctx, id)
			if err != nil {
				return "", fmt.Errorf("get session: %w", err)
			}
			data, _ := json.MarshalIndent(info, "", "  ")
			return string(data), nil
		},
	}
}

func getMessagesTool(tp ToolProvider) *tools.Tool {
	return &tools.Tool{
		Name:        "get_messages",
		Description: "Get the conversation history of a session. Returns the last N messages (default 3).",
		Schema: tools.InputSchema{
			Properties: map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "The session ID or prefix.",
				},
				"last_n": map[string]any{
					"type":        "integer",
					"description": "Number of recent messages to return. Default 3.",
				},
			},
			Required: []string{"session_id"},
		},
		Effect: tools.ReadOnly,
		Fn: func(input json.RawMessage) (string, error) {
			var args struct {
				SessionID string `json:"session_id"`
				LastN     int    `json:"last_n"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if args.LastN <= 0 {
				args.LastN = 3
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			id, err := resolveSessionID(ctx, tp, args.SessionID)
			if err != nil {
				return "", err
			}

			messages, err := tp.GetSessionMessages(ctx, id)
			if err != nil {
				return "", fmt.Errorf("get messages: %w", err)
			}

			// Trim to last N.
			if len(messages) > args.LastN {
				messages = messages[len(messages)-args.LastN:]
			}

			if len(messages) == 0 {
				return "No messages in this session.", nil
			}

			var b strings.Builder
			for _, m := range messages {
				fmt.Fprintf(&b, "[%s] %s\n", m.Role, truncate(m.Content, 500))
				for _, p := range m.Parts {
					if p.Type == agent.PartToolCall {
						fmt.Fprintf(&b, "  -> tool_call: %s\n", p.Tool)
					}
				}
			}
			return b.String(), nil
		},
	}
}

func sendMessageTool(tp ToolProvider) *tools.Tool {
	return &tools.Tool{
		Name:        "send_message",
		Description: "Send a follow-up message to a coding agent session. Use this to relay user decisions, answer agent questions, or provide feedback on plans.",
		Schema: tools.InputSchema{
			Properties: map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "The session ID or prefix.",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "The message text to send to the agent.",
				},
			},
			Required: []string{"session_id", "text"},
		},
		Effect: tools.WriteEffect,
		Fn: func(input json.RawMessage) (string, error) {
			var args struct {
				SessionID string `json:"session_id"`
				Text      string `json:"text"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if args.Text == "" {
				return "", fmt.Errorf("text is required")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			id, err := resolveSessionID(ctx, tp, args.SessionID)
			if err != nil {
				return "", err
			}

			if err := tp.SendMessage(ctx, id, args.Text); err != nil {
				return "", fmt.Errorf("send message: %w", err)
			}
			return fmt.Sprintf("Message sent to session %s.", id[:8]), nil
		},
	}
}

func createSessionTool(tp ToolProvider) *tools.Tool {
	return &tools.Tool{
		Name:        "create_session",
		Description: "Create a new coding agent session with the given backend, project directory, and prompt.",
		Schema: tools.InputSchema{
			Properties: map[string]any{
				"backend": map[string]any{
					"type":        "string",
					"description": "Backend to use: 'opencode' or 'claude-code'.",
					"enum":        []string{"opencode", "claude-code"},
				},
				"project_dir": map[string]any{
					"type":        "string",
					"description": "Absolute path to the project directory.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task prompt for the coding agent.",
				},
			},
			Required: []string{"backend", "project_dir", "prompt"},
		},
		Effect: tools.WriteEffect,
		Fn: func(input json.RawMessage) (string, error) {
			var args struct {
				Backend    string `json:"backend"`
				ProjectDir string `json:"project_dir"`
				Prompt     string `json:"prompt"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			info, err := tp.CreateSession(ctx, agent.StartRequest{
				Backend:    agent.BackendType(args.Backend),
				ProjectDir: args.ProjectDir,
				Prompt:     args.Prompt,
			})
			if err != nil {
				return "", fmt.Errorf("create session: %w", err)
			}
			return fmt.Sprintf("Session created: %s (%s)", info.ID[:8], info.ProjectName), nil
		},
	}
}

func abortSessionTool(tp ToolProvider) *tools.Tool {
	return &tools.Tool{
		Name:        "abort_session",
		Description: "Abort/interrupt a running coding agent session.",
		Schema: tools.InputSchema{
			Properties: map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "The session ID or prefix.",
				},
			},
			Required: []string{"session_id"},
		},
		Effect: tools.WriteEffect,
		Fn: func(input json.RawMessage) (string, error) {
			var args struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			id, err := resolveSessionID(ctx, tp, args.SessionID)
			if err != nil {
				return "", err
			}

			if err := tp.AbortSession(ctx, id); err != nil {
				return "", fmt.Errorf("abort session: %w", err)
			}
			return fmt.Sprintf("Session %s aborted.", id[:8]), nil
		},
	}
}

// resolveSessionID matches a prefix to a full session ID by listing
// sessions and finding a unique prefix match.
func resolveSessionID(ctx context.Context, tp ToolProvider, prefix string) (string, error) {
	if prefix == "" {
		return "", fmt.Errorf("session_id is required")
	}
	// Try exact match first via GetSession.
	info, err := tp.GetSession(ctx, prefix)
	if err == nil && info != nil {
		return info.ID, nil
	}

	// Fall back to prefix match.
	sessions, err := tp.ListSessions(ctx)
	if err != nil {
		return "", fmt.Errorf("list sessions for prefix resolution: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, prefix) {
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d sessions", prefix, len(matches))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
