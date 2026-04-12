package clankcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
)

func sessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sessions",
		Aliases: []string{"s"},
		Short:   "Manage daemon sessions",
		Long:    "List, inspect, and control coding agent sessions on the Clank daemon. All output is JSON.",
	}

	cmd.AddCommand(
		sessionsListCmd(),
		sessionsGetCmd(),
		sessionsMessagesCmd(),
		sessionsSendCmd(),
		sessionsNewCmd(),
		sessionsAbortCmd(),
	)

	return cmd
}

// --- clank sessions list ---

func sessionsListCmd() *cobra.Command {
	var query string
	var since string
	var until string
	var visibility string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions (with optional filters)",
		Long: `List coding agent sessions. By default shows active (unfinished) sessions.

Use --query to filter by text (pipe-separated OR groups, space-separated AND terms).
Use --since/--until to filter by time (relative durations like '7d'/'24h' or RFC 3339).
Use --visibility to include done/archived sessions.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// If no filters specified, list all active sessions.
			if query == "" && since == "" && until == "" && visibility == "" {
				sessions, err := client.ListSessions(ctx)
				if err != nil {
					return fmt.Errorf("list sessions: %w", err)
				}
				return writeJSONOut(sessions)
			}

			var p agent.SearchParams
			p.Query = query
			p.Visibility = agent.SessionVisibility(visibility)

			if since != "" {
				t, err := agent.ParseTimeParam(since)
				if err != nil {
					return fmt.Errorf("invalid --since: %w", err)
				}
				p.Since = t
			}
			if until != "" {
				t, err := agent.ParseTimeParam(until)
				if err != nil {
					return fmt.Errorf("invalid --until: %w", err)
				}
				p.Until = t
			}

			sessions, err := client.SearchSessions(ctx, p)
			if err != nil {
				return fmt.Errorf("search sessions: %w", err)
			}
			return writeJSONOut(sessions)
		},
	}

	cmd.Flags().StringVarP(&query, "query", "q", "", "Search query (pipe-separated OR groups, space-separated AND)")
	cmd.Flags().StringVar(&since, "since", "", "Only sessions updated after this time (e.g. '7d', '24h', RFC 3339)")
	cmd.Flags().StringVar(&until, "until", "", "Only sessions updated before this time")
	cmd.Flags().StringVar(&visibility, "visibility", "", "Filter: '' (active), 'all', 'done', 'archived'")

	return cmd
}

// --- clank sessions get ---

func sessionsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <session-id>",
		Short: "Get detailed info about a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			info, err := client.GetSession(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get session: %w", err)
			}
			return writeJSONOut(info)
		},
	}
}

// --- clank sessions messages ---

func sessionsMessagesCmd() *cobra.Command {
	var lastN int

	cmd := &cobra.Command{
		Use:   "messages <session-id>",
		Short: "Get conversation history of a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			messages, err := client.GetSessionMessages(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get messages: %w", err)
			}

			if lastN > 0 && len(messages) > lastN {
				messages = messages[len(messages)-lastN:]
			}
			return writeJSONOut(messages)
		},
	}

	cmd.Flags().IntVarP(&lastN, "last", "n", 0, "Return only the last N messages (0 = all)")

	return cmd
}

// --- clank sessions send ---

func sessionsSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <session-id> <text>...",
		Short: "Send a follow-up message to a session",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			sessionID := args[0]
			text := strings.Join(args[1:], " ")

			if err := client.SendMessage(ctx, sessionID, agent.SendMessageOpts{Text: text}); err != nil {
				return fmt.Errorf("send message: %w", err)
			}
			return writeJSONOut(map[string]string{
				"status":     "sent",
				"session_id": sessionID,
			})
		},
	}
}

// --- clank sessions new ---

func sessionsNewCmd() *cobra.Command {
	var backend string
	var projectDir string
	var worktreeBranch string

	cmd := &cobra.Command{
		Use:   "new <prompt>...",
		Short: "Create a new coding agent session",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if projectDir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				projectDir = cwd
			}

			bt := agent.BackendOpenCode
			if backend == "claude" || backend == "claude-code" {
				bt = agent.BackendClaudeCode
			} else if backend != "" && backend != "opencode" {
				return fmt.Errorf("unknown backend: %s (valid: opencode, claude-code)", backend)
			}

			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			prompt := strings.Join(args, " ")
			info, err := client.CreateSession(ctx, agent.StartRequest{
				Backend:        bt,
				ProjectDir:     projectDir,
				WorktreeBranch: worktreeBranch,
				Prompt:         prompt,
			})
			if err != nil {
				return fmt.Errorf("create session: %w", err)
			}
			return writeJSONOut(info)
		},
	}

	cmd.Flags().StringVar(&backend, "backend", "", "Backend: opencode (default), claude-code")
	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: cwd)")
	cmd.Flags().StringVar(&worktreeBranch, "worktree", "", "Git branch to work on (creates worktree if needed)")
	cmd.Flags().StringVar(&worktreeBranch, "branch", "", "Git branch to work on (creates worktree if needed)")
	_ = cmd.Flags().MarkHidden("branch") // hidden alias for familiarity

	return cmd
}

// --- clank sessions abort ---

func sessionsAbortCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abort <session-id>",
		Short: "Abort a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := client.AbortSession(ctx, args[0]); err != nil {
				return fmt.Errorf("abort session: %w", err)
			}
			return writeJSONOut(map[string]string{
				"status":     "aborted",
				"session_id": args[0],
			})
		},
	}
}

// writeJSONOut marshals v as indented JSON to stdout.
func writeJSONOut(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
