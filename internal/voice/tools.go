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
	SearchSessions(ctx context.Context, p agent.SearchParams) ([]agent.SessionInfo, error)
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
		Name: "list_sessions",
		Description: "List coding agent sessions. By default only shows active (unfinished) sessions. " +
			"Use 'query' to filter by text (pipe-separated OR groups, space-separated AND terms). " +
			"Use 'since'/'until' to filter by time (relative durations like '7d'/'24h' or RFC 3339 timestamps). " +
			"Use 'visibility' to include done/archived sessions. ",
		Schema: tools.InputSchema{
			Properties: map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query. Pipe-separated OR groups with space-separated AND terms. E.g. 'auth bug|dark mode'. Parenthesis not supported.",
				},
				"since": map[string]any{
					"type":        "string",
					"description": "Only sessions updated at or after this time. Relative duration (e.g. '7d', '24h') or RFC 3339 timestamp.",
				},
				"until": map[string]any{
					"type":        "string",
					"description": "Only sessions updated before this time. Relative duration (e.g. '7d', '24h') or RFC 3339 timestamp.",
				},
				"visibility": map[string]any{
					"type":        "string",
					"description": "Filter by visibility. '' (default) = active only, 'all' = everything, 'done' = done only, 'archived' = archived only.",
					"enum":        []string{"", "all", "done", "archived"},
				},
			},
		},
		Effect: tools.ReadOnly,
		Fn: func(input json.RawMessage) (string, error) {
			var args struct {
				Query      string `json:"query"`
				Since      string `json:"since"`
				Until      string `json:"until"`
				Visibility string `json:"visibility"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}

			var p agent.SearchParams
			p.Query = args.Query
			p.Visibility = agent.SessionVisibility(args.Visibility)

			if args.Since != "" {
				t, err := time.Parse(time.RFC3339, args.Since)
				if err != nil {
					return "", fmt.Errorf("invalid since: %w", err)
				}
				p.Since = t
			}
			if args.Until != "" {
				t, err := time.Parse(time.RFC3339, args.Until)
				if err != nil {
					return "", fmt.Errorf("invalid until: %w", err)
				}
				p.Until = t
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sessions, err := tp.SearchSessions(ctx, p)
			if err != nil {
				return "", fmt.Errorf("search sessions: %w", err)
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
		Description: "Get detailed information about a specific session by its full ID. Use list_sessions first to find the ID.",
		Schema: tools.InputSchema{
			Properties: map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "The full session ID. Use list_sessions to find it.",
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
					"description": "The full session ID. Use list_sessions to find it.",
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
					"description": "The full session ID. Use list_sessions to find it.",
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
					"description": "The full session ID. Use list_sessions to find it.",
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

// resolveSessionID looks up a session by its full ID. Partial IDs are not
// supported — if the exact ID is not found, the error directs the caller to
// use list_sessions to find the correct full ID.
func resolveSessionID(ctx context.Context, tp ToolProvider, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("session_id is required")
	}
	info, err := tp.GetSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("session %q not found — use list_sessions to find the correct full session ID", id)
	}
	return info.ID, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
