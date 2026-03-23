package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/analyzer"
	"github.com/acksell/clank/internal/config"
	clankctx "github.com/acksell/clank/internal/context"
	"github.com/acksell/clank/internal/llm"
	"github.com/acksell/clank/internal/scanner"
	"github.com/acksell/clank/internal/scanner/opencode"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/internal/tui"
)

func main() {
	root := &cobra.Command{
		Use:   "clank",
		Short: "AI-powered coding session backlog triager",
		Long:  "Clank scans your coding sessions, extracts unfinished threads and opportunities, and helps you triage them.",
	}

	root.AddCommand(
		scanCmd(),
		triageCmd(),
		listCmd(),
		showCmd(),
		contextCmd(),
		repoCmd(),
		initCmd(),
		configCmd(),
		backfillCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func openStore() (*store.Store, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, "clank.db"))
}

func newLLMClient(cfg config.Config) *llm.Client {
	apiKey := cfg.LLM.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil
	}
	return llm.NewClient(cfg.LLM.BaseURL, apiKey, cfg.LLM.Model)
}

func scanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan [repo-path...]",
		Short: "Scan coding sessions and extract tickets",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			client := newLLMClient(cfg)
			if client == nil {
				return fmt.Errorf("no LLM API key configured. Set llm.api_key in ~/.clank/config.toml or OPENAI_API_KEY env var")
			}

			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			sc := opencode.New(cfg.Scan.OpenCodeDB)
			az := analyzer.New(client)
			ctx, _ := clankctx.LoadAll()

			repos := args
			if len(repos) == 0 {
				// Auto-discover all repos from the opencode database
				projects, err := sc.ListProjects()
				if err != nil {
					return fmt.Errorf("list projects: %w", err)
				}
				repos = projects
			}

			if len(repos) == 0 {
				fmt.Println("No repos found. Run a coding session with opencode first, or pass a repo path.")
				return nil
			}

			totalTickets := 0
			for _, repoPath := range repos {
				absPath, err := filepath.Abs(repoPath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not resolve %s: %v\n", repoPath, err)
					continue
				}

				repo, err := s.GetRepo(absPath)
				afterID := ""
				if err == nil {
					afterID = repo.LastSessionID
				}

				sessions, err := sc.Scan(absPath, afterID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: scan %s: %v\n", absPath, err)
					continue
				}

				if len(sessions) == 0 {
					continue
				}

				fmt.Printf("Scanning %s (%d new sessions)\n", filepath.Base(absPath), len(sessions))
				count, lastID := processSessions(s, az, sessions, ctx)
				totalTickets += count

				if lastID != "" {
					s.SaveRepo(&store.Repo{
						Path:          absPath,
						Name:          filepath.Base(absPath),
						LastScanAt:    time.Now(),
						LastSessionID: lastID,
					})
				}
			}

			fmt.Printf("Scan complete. Extracted %d new tickets.\n", totalTickets)
			return nil
		},
	}
	return cmd
}

func processSessions(s *store.Store, az *analyzer.Analyzer, sessions []scanner.RawSession, ctx string) (int, string) {
	totalTickets := 0
	lastID := ""
	for i, sess := range sessions {
		fmt.Printf("  [%d/%d] Analyzing: %s...\n", i+1, len(sessions), truncStr(sess.Title, 60))
		tickets, err := az.Analyze(sess, ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: analyze %s: %v\n", sess.ID, err)
			continue
		}
		for j := range tickets {
			if err := s.SaveTicket(&tickets[j]); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: save ticket: %v\n", err)
			} else {
				totalTickets++
			}
		}
		lastID = sess.ID
	}
	return totalTickets, lastID
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func triageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "triage",
		Short: "Open interactive TUI for triaging tickets",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			client := newLLMClient(cfg)
			var az *analyzer.Analyzer
			if client != nil {
				az = analyzer.New(client)
			}

			ctx, _ := clankctx.LoadAll()
			app := tui.NewApp(s, az, ctx)
			p := tea.NewProgram(app, tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}
}

func listCmd() *cobra.Command {
	var repo, status, label, ticketType, quadrant string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tickets",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			tickets, err := s.ListTickets(store.TicketFilter{
				RepoPath: repo,
				Status:   store.TicketStatus(status),
				Label:    label,
				Type:     store.TicketType(ticketType),
				Quadrant: store.Quadrant(quadrant),
			})
			if err != nil {
				return err
			}

			if len(tickets) == 0 {
				fmt.Println("No tickets found. Run 'clank scan' first.")
				return nil
			}

			for _, t := range tickets {
				repoName := filepath.Base(t.RepoPath)
				q := string(t.Quadrant())
				if q == "" {
					q = "—"
				}
				fmt.Printf("[%s] %-10s %-8s %-11s %s (%s)\n",
					t.ID[:8], t.Status, shortTyp(string(t.Type)), q, t.Title, repoName)
			}
			fmt.Printf("\n%d tickets total\n", len(tickets))
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "Filter by repo path")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status")
	cmd.Flags().StringVar(&label, "label", "", "Filter by label")
	cmd.Flags().StringVar(&ticketType, "type", "", "Filter by type")
	cmd.Flags().StringVar(&quadrant, "quadrant", "", "Filter by quadrant (quickwin, valuebet, distraction, tidyup)")
	return cmd
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <ticket-id>",
		Short: "Show ticket detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			t, err := s.GetTicket(args[0])
			if err == sql.ErrNoRows {
				tickets, err := s.ListTickets(store.TicketFilter{})
				if err != nil {
					return err
				}
				for _, ticket := range tickets {
					if strings.HasPrefix(ticket.ID, args[0]) {
						t = &ticket
						break
					}
				}
				if t == nil {
					return fmt.Errorf("ticket not found: %s", args[0])
				}
			} else if err != nil {
				return err
			}

			fmt.Printf("ID:          %s\n", t.ID)
			fmt.Printf("Type:        %s\n", t.Type)
			fmt.Printf("Status:      %s\n", t.Status)
			fmt.Printf("Title:       %s\n", t.Title)
			fmt.Printf("Summary:     %s\n", t.Summary)
			fmt.Printf("Description: %s\n", t.Description)
			fmt.Printf("Repo:        %s\n", t.RepoPath)
			fmt.Printf("Session:     %s (%s)\n", t.SessionTitle, t.SessionID)
			fmt.Printf("Date:        %s\n", t.SessionDate.Format("2006-01-02 15:04"))
			fmt.Printf("Complexity:  %d/5\n", t.Complexity)
			fmt.Printf("Impact:      %d/5\n", t.Impact)
			q := string(t.Quadrant())
			if q == "" {
				q = "(unscored)"
			}
			fmt.Printf("Quadrant:    %s\n", q)
			fmt.Printf("Labels:      %s\n", strings.Join(t.Labels, ", "))
			if t.AINotes != "" {
				fmt.Printf("\nAI Notes:\n%s\n", t.AINotes)
			}
			if t.UserNotes != "" {
				fmt.Printf("\nYour Notes:\n%s\n", t.UserNotes)
			}
			if len(t.SourceQuotes) > 0 {
				fmt.Println("\nSource Quotes:")
				for _, q := range t.SourceQuotes {
					fmt.Printf("  > %s\n", q)
				}
			}
			return nil
		},
	}
}

func contextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "context",
		Short: "Open central context directory in your editor",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clankctx.Init(); err != nil {
				return err
			}
			dir, err := clankctx.Dir()
			if err != nil {
				return err
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}
			c := exec.Command(editor, dir)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}
}

func repoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage registered repositories",
	}

	addCmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a repository for scanning",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			absPath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			err = s.SaveRepo(&store.Repo{
				Path: absPath,
				Name: filepath.Base(absPath),
			})
			if err != nil {
				return err
			}
			fmt.Printf("Registered repo: %s\n", absPath)
			return nil
		},
	}

	lsCmd := &cobra.Command{
		Use:   "list",
		Short: "List registered repositories",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			repos, err := s.ListRepos()
			if err != nil {
				return err
			}
			if len(repos) == 0 {
				fmt.Println("No repos registered. Use 'clank repo add <path>' or 'clank init'.")
				return nil
			}
			for _, r := range repos {
				lastScan := "never"
				if !r.LastScanAt.IsZero() && r.LastScanAt.UnixMilli() > 0 {
					lastScan = r.LastScanAt.Format("2006-01-02 15:04")
				}
				fmt.Printf("%-40s (last scan: %s)\n", r.Path, lastScan)
			}
			return nil
		},
	}

	cmd.AddCommand(addCmd, lsCmd)
	return cmd
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Register the current directory as a repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			err = s.SaveRepo(&store.Repo{
				Path: cwd,
				Name: filepath.Base(cwd),
			})
			if err != nil {
				return err
			}
			if err := clankctx.Init(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not init context: %v\n", err)
			}
			fmt.Printf("Initialized clank for: %s\n", cwd)
			fmt.Println("Run 'clank scan .' to scan this repo's sessions.")
			return nil
		},
	}
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or set configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			p, _ := config.Path()
			fmt.Printf("Config file: %s\n\n", p)

			apiKey := cfg.LLM.APIKey
			if apiKey == "" {
				apiKey = os.Getenv("OPENAI_API_KEY")
				if apiKey != "" {
					apiKey = "(from OPENAI_API_KEY env)"
				} else {
					apiKey = "(not set)"
				}
			} else if len(apiKey) > 8 {
				apiKey = apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
			}

			fmt.Printf("LLM Base URL:  %s\n", cfg.LLM.BaseURL)
			fmt.Printf("LLM Model:     %s\n", cfg.LLM.Model)
			fmt.Printf("LLM API Key:   %s\n", apiKey)
			fmt.Printf("OpenCode DB:   %s\n", cfg.Scan.OpenCodeDB)
			fmt.Printf("Repos:         %d registered\n", len(cfg.Repos))
			return nil
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a config value (e.g., llm.api_key, llm.model, llm.base_url)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			switch args[0] {
			case "llm.api_key":
				cfg.LLM.APIKey = args[1]
			case "llm.model":
				cfg.LLM.Model = args[1]
			case "llm.base_url":
				cfg.LLM.BaseURL = args[1]
			case "scan.opencode_db":
				cfg.Scan.OpenCodeDB = args[1]
			default:
				return fmt.Errorf("unknown config key: %s\nValid keys: llm.api_key, llm.model, llm.base_url, scan.opencode_db", args[0])
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Printf("Set %s\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(setCmd)
	return cmd
}

func shortTyp(t string) string {
	switch t {
	case "unfinished_thread":
		return "thread"
	case "opportunity":
		return "oppty"
	default:
		return t
	}
}

func backfillCmd() *cobra.Command {
	var repo string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Backfill impact scores for tickets missing them",
		Long:  "Uses the LLM to score the impact (1-5) of tickets that have impact=0. This is useful for scoring existing tickets after adding the impact dimension.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			client := newLLMClient(cfg)
			if client == nil {
				return fmt.Errorf("no LLM API key configured. Set llm.api_key in ~/.clank/config.toml or OPENAI_API_KEY env var")
			}

			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			az := analyzer.New(client)
			ctx, _ := clankctx.LoadAll()

			filter := store.TicketFilter{RepoPath: repo}
			tickets, err := s.ListTickets(filter)
			if err != nil {
				return err
			}

			// Filter to tickets with no impact score.
			var unscored []store.Ticket
			for _, t := range tickets {
				if t.Impact == 0 {
					unscored = append(unscored, t)
				}
			}

			if len(unscored) == 0 {
				fmt.Println("All tickets already have impact scores.")
				return nil
			}

			fmt.Printf("Found %d tickets without impact scores.\n", len(unscored))
			if dryRun {
				fmt.Println("Dry run — no changes will be saved.")
			}

			scored := 0
			errors := 0
			for i, t := range unscored {
				fmt.Printf("  [%d/%d] Scoring: %s...", i+1, len(unscored), truncStr(t.Title, 50))
				impact, err := az.ScoreImpact(t, ctx)
				if err != nil {
					fmt.Printf(" error: %v\n", err)
					errors++
					continue
				}
				fmt.Printf(" impact=%d quadrant=%s\n", impact, quadrantLabel(impact, t.Complexity))

				if !dryRun {
					t.Impact = impact
					if err := s.SaveTicket(&t); err != nil {
						fmt.Printf("    save error: %v\n", err)
						errors++
						continue
					}
				}
				scored++
			}

			fmt.Printf("\nBackfill complete. Scored %d tickets", scored)
			if errors > 0 {
				fmt.Printf(", %d errors", errors)
			}
			if dryRun {
				fmt.Printf(" (dry run)")
			}
			fmt.Println(".")
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "Only backfill tickets for a specific repo")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview scores without saving")
	return cmd
}

func quadrantLabel(impact, complexity int) string {
	if impact == 0 || complexity == 0 {
		return "unscored"
	}
	hi := impact >= 3
	hc := complexity >= 3
	switch {
	case hi && !hc:
		return "quickwin"
	case hi && hc:
		return "valuebet"
	case !hi && hc:
		return "distraction"
	default:
		return "tidyup"
	}
}
