package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/analyzer"
	"github.com/acksell/clank/internal/config"
	clankctx "github.com/acksell/clank/internal/context"
	"github.com/acksell/clank/internal/daemon"
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
		sessionsCmd(),
		listCmd(),
		showCmd(),
		contextCmd(),
		repoCmd(),
		initCmd(),
		configCmd(),
		backfillCmd(),
		daemonCmd(),
		codeCmd(),
		inboxCmd(),
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

func newLLMClient(cfg config.Config) (*llm.Client, error) {
	if cfg.LLM.ResolveAPIKey() == "" {
		return nil, fmt.Errorf("no API key configured. Run 'clank config' to set up your LLM provider")
	}
	return llm.NewClient(cfg.LLM)
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

			client, err := newLLMClient(cfg)
			if err != nil {
				return fmt.Errorf("LLM client: %w", err)
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

			client, err := newLLMClient(cfg)
			var az *analyzer.Analyzer
			if err == nil {
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

func sessionsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Interactive session dashboard (kanban board)",
		Long:  "View and manage your coding sessions in a kanban-style board. Shows idle, busy, error, and follow-up sessions, plus top backlog tickets.",
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

			sc := opencode.New(cfg.Scan.OpenCodeDB)
			app := tui.NewSessionsModel(s, sc, limit)
			p := tea.NewProgram(app, tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "Maximum number of recent sessions to show")
	return cmd
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
			fmt.Printf("Complexity:  %d/10\n", t.Complexity)
			fmt.Printf("Impact:      %d/10\n", t.Impact)
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
		Short: "Configure clank interactively",
		Long:  "Run the interactive setup form to configure your LLM provider, or use 'config show' to view and 'config set' to update individual values.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			// Build provider options, annotating those with detected env keys.
			detected := config.DetectEnvKeys()
			providers := config.Providers()
			providerOpts := make([]huh.Option[string], 0, len(providers))
			for _, p := range providers {
				label := p.Name
				if _, ok := detected[p.ID]; ok {
					label += fmt.Sprintf("  (found %s)", p.EnvKey)
				}
				providerOpts = append(providerOpts, huh.NewOption(label, p.ID))
			}

			// Pre-fill from existing config (or defaults).
			provider := cfg.LLM.Provider
			if provider == "" {
				provider = config.ProviderOpenAI
			}
			apiKey := cfg.LLM.APIKey
			model := cfg.LLM.Model
			openCodeDB := cfg.Scan.OpenCodeDB

			// Step 1: Pick provider.
			form1 := huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("LLM Provider").
						Options(providerOpts...).
						Value(&provider),
				),
			)
			if err := form1.Run(); err != nil {
				return err
			}

			// After provider is chosen, set model default if empty or if it
			// belongs to a different provider.
			pInfo, _ := config.ProviderByID(provider)
			if model == "" || !isModelForProvider(model, provider) {
				model = pInfo.DefaultModel
			}

			// Pre-fill API key from env if not already set.
			envKey := detected[provider]
			apiKeyDescription := "Paste your API key"
			if envKey != "" && apiKey == "" {
				apiKeyDescription = fmt.Sprintf("Using %s from environment. Press enter to keep, or paste a new key", pInfo.EnvKey)
			}

			// Step 2: API key + model + opencode DB.
			form2 := huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title("API Key").
						Description(apiKeyDescription).
						EchoMode(huh.EchoModePassword).
						Value(&apiKey),

					huh.NewInput().
						Title("Model").
						Description("Which model to use").
						Value(&model).
						Validate(huh.ValidateNotEmpty()),

					huh.NewInput().
						Title("OpenCode DB Path").
						Description("Path to the opencode database").
						Value(&openCodeDB).
						Validate(huh.ValidateNotEmpty()),
				),
			)
			if err := form2.Run(); err != nil {
				return err
			}

			// If user left API key blank but env var exists, don't store it
			// (it'll be resolved from env at runtime).
			cfg.LLM.Provider = provider
			cfg.LLM.APIKey = apiKey
			cfg.LLM.Model = model
			cfg.Scan.OpenCodeDB = openCodeDB

			if err := config.Save(cfg); err != nil {
				return err
			}

			p, _ := config.Path()
			fmt.Printf("\nConfig saved to %s\n", p)

			// Show what will be used at runtime.
			resolvedKey := cfg.LLM.ResolveAPIKey()
			if resolvedKey == "" {
				fmt.Printf("\nWarning: no API key configured. Set %s or re-run 'clank config'.\n", pInfo.EnvKey)
			} else {
				source := "config"
				if apiKey == "" {
					source = pInfo.EnvKey + " env"
				}
				fmt.Printf("API key: (%s)\n", source)
			}
			return nil
		},
	}

	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show current configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			p, _ := config.Path()
			fmt.Printf("Config file: %s\n\n", p)

			providerLabel := cfg.LLM.Provider
			if pInfo, ok := config.ProviderByID(cfg.LLM.Provider); ok {
				providerLabel = pInfo.Name
			}

			resolvedKey := cfg.LLM.ResolveAPIKey()
			apiKeyDisplay := "(not set)"
			if resolvedKey != "" {
				if cfg.LLM.APIKey != "" {
					if len(resolvedKey) > 8 {
						apiKeyDisplay = resolvedKey[:4] + "..." + resolvedKey[len(resolvedKey)-4:]
					} else {
						apiKeyDisplay = "****"
					}
				} else if pInfo, ok := config.ProviderByID(cfg.LLM.Provider); ok {
					apiKeyDisplay = fmt.Sprintf("(from %s env)", pInfo.EnvKey)
				}
			}

			fmt.Printf("Provider:      %s\n", providerLabel)
			fmt.Printf("Model:         %s\n", cfg.LLM.Model)
			fmt.Printf("API Key:       %s\n", apiKeyDisplay)
			if cfg.LLM.BaseURL != "" {
				fmt.Printf("Base URL:      %s\n", cfg.LLM.BaseURL)
			}
			fmt.Printf("OpenCode DB:   %s\n", cfg.Scan.OpenCodeDB)
			fmt.Printf("Repos:         %d registered\n", len(cfg.Repos))
			return nil
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a config value (e.g., llm.provider, llm.api_key, llm.model, llm.base_url)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			switch args[0] {
			case "llm.provider":
				if _, ok := config.ProviderByID(args[1]); !ok {
					providers := config.Providers()
					ids := make([]string, len(providers))
					for i, p := range providers {
						ids[i] = p.ID
					}
					return fmt.Errorf("unknown provider: %s\nValid providers: %s", args[1], strings.Join(ids, ", "))
				}
				cfg.LLM.Provider = args[1]
			case "llm.api_key":
				cfg.LLM.APIKey = args[1]
			case "llm.model":
				cfg.LLM.Model = args[1]
			case "llm.base_url":
				cfg.LLM.BaseURL = args[1]
			case "scan.opencode_db":
				cfg.Scan.OpenCodeDB = args[1]
			default:
				return fmt.Errorf("unknown config key: %s\nValid keys: llm.provider, llm.api_key, llm.model, llm.base_url, scan.opencode_db", args[0])
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Printf("Set %s\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(showCmd, setCmd)
	return cmd
}

// isModelForProvider does a rough check of whether a model name
// belongs to the given provider, to decide if we should reset the
// default when switching providers.
func isModelForProvider(model, provider string) bool {
	switch provider {
	case config.ProviderOpenAI:
		return strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")
	case config.ProviderAnthropic:
		return strings.HasPrefix(model, "claude-")
	case config.ProviderGemini:
		return strings.HasPrefix(model, "gemini-")
	default:
		return true
	}
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
		Long:  "Uses the LLM to score the impact (1-10) of tickets that have impact=0. This is useful for scoring existing tickets after adding the impact dimension.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			client, err := newLLMClient(cfg)
			if err != nil {
				return fmt.Errorf("LLM client: %w", err)
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

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the Clank background daemon",
		Long:  "The daemon manages coding agent sessions in the background. It starts automatically when needed.",
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the background daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			foreground, _ := cmd.Flags().GetBool("foreground")
			return runDaemonStart(foreground)
		},
	}
	startCmd.Flags().Bool("foreground", false, "Run in foreground (don't daemonize)")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStop()
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status and managed sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStatus()
		},
	}

	cmd.AddCommand(startCmd, stopCmd, statusCmd)
	return cmd
}

// runDaemonStart starts the daemon, either in foreground or as a background process.
func runDaemonStart(foreground bool) error {
	running, pid, err := daemon.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if running {
		fmt.Printf("Daemon already running (pid=%d)\n", pid)
		return nil
	}

	if foreground {
		// Run in foreground — useful for debugging.
		d, err := daemon.New()
		if err != nil {
			return err
		}
		// Wire in real backend factory.
		factory := daemon.NewDefaultBackendFactory()
		d.BackendFactory = factory.Create
		d.OnShutdown = factory.StopAll
		return d.Run()
	}

	// Fork a background process.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	bgCmd := exec.Command(exe, "daemon", "start", "--foreground")
	bgCmd.Stdout = nil
	bgCmd.Stderr = nil
	bgCmd.Stdin = nil
	// Start in a new process group so it doesn't get signals from our terminal.
	bgCmd.SysProcAttr = daemonSysProcAttr()

	if err := bgCmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Wait briefly for the daemon to be reachable.
	client, err := daemon.NewDefaultClient()
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for {
		if err := client.Ping(ctx); err == nil {
			fmt.Printf("Daemon started (pid=%d)\n", bgCmd.Process.Pid)
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Printf("Daemon process started (pid=%d) but not yet reachable\n", bgCmd.Process.Pid)
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// runDaemonStop sends SIGTERM to the running daemon.
func runDaemonStop() error {
	running, pid, err := daemon.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if !running {
		fmt.Println("Daemon is not running")
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	if err := proc.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("signal daemon (pid=%d): %w", pid, err)
	}

	// Wait for it to exit.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for {
		stillRunning, _, _ := daemon.IsRunning()
		if !stillRunning {
			fmt.Printf("Daemon stopped (was pid=%d)\n", pid)
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Printf("Daemon may still be shutting down (pid=%d)\n", pid)
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runDaemonStatus shows daemon info and managed sessions.
func runDaemonStatus() error {
	running, pid, err := daemon.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if !running {
		fmt.Println("Daemon is not running")
		return nil
	}

	client, err := daemon.NewDefaultClient()
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := client.Status(ctx)
	if err != nil {
		// Daemon process exists but API not reachable.
		fmt.Printf("Daemon process exists (pid=%d) but API is not reachable: %v\n", pid, err)
		return nil
	}

	fmt.Printf("Daemon running (pid=%d, uptime=%s)\n", status.PID, status.Uptime)
	if len(status.Sessions) == 0 {
		fmt.Println("No managed sessions")
	} else {
		fmt.Printf("\n%d managed session(s):\n", len(status.Sessions))
		for _, s := range status.Sessions {
			prompt := s.Prompt
			if len(prompt) > 50 {
				prompt = prompt[:47] + "..."
			}
			fmt.Printf("  [%s] %-8s %-12s %s\n", s.ID[:8], s.Status, s.ProjectName, prompt)
		}
	}
	return nil
}

func quadrantLabel(impact, complexity int) string {
	if impact == 0 || complexity == 0 {
		return "unscored"
	}
	hi := impact >= 6
	hc := complexity >= 6
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

// --- clank code ---

func codeCmd() *cobra.Command {
	var backend string
	var projectDir string
	var ticketID string

	cmd := &cobra.Command{
		Use:   "code [prompt]",
		Short: "Launch a new coding agent session",
		Long: `Launch a new coding agent session managed by the Clank daemon.

If a prompt is provided, the session starts immediately and opens the
session detail TUI. Without a prompt, opens the inbox TUI.

The daemon is auto-started if not already running.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Determine prompt.
			prompt := strings.Join(args, " ")
			if prompt == "" {
				// No prompt — open new session dialog.
				return runNewSession()
			}

			// Determine project directory.
			if projectDir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				projectDir = cwd
			}

			// Resolve backend type.
			bt := agent.BackendOpenCode // default
			if backend == "claude" || backend == "claude-code" {
				bt = agent.BackendClaudeCode
			} else if backend != "" && backend != "opencode" {
				return fmt.Errorf("unknown backend: %s (valid: opencode, claude)", backend)
			}

			// Ensure daemon is running.
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			// Subscribe to SSE BEFORE creating the session so we don't miss
			// events emitted during session startup.
			sseCtx, sseCancel := context.WithCancel(context.Background())
			events, err := client.SubscribeEvents(sseCtx)
			if err != nil {
				sseCancel()
				return fmt.Errorf("subscribe events: %w", err)
			}

			// Create the session.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			info, err := client.CreateSession(ctx, agent.StartRequest{
				Backend:    bt,
				ProjectDir: projectDir,
				Prompt:     prompt,
				TicketID:   ticketID,
			})
			if err != nil {
				sseCancel()
				return fmt.Errorf("create session: %w", err)
			}

			// Open session detail TUI with pre-connected event channel.
			model := tui.NewSessionViewModel(client, info.ID)
			model.SetStandalone(true)
			model.SetEventChannel(events, sseCancel)
			p := tea.NewProgram(model, tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}

	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&ticketID, "ticket", "", "Link to backlog ticket ID")

	return cmd
}

func inboxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inbox",
		Short: "Open the agent session inbox",
		Long:  "View and manage daemon-managed coding agent sessions in an interactive TUI.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInbox()
		},
	}
}

// runInbox opens the inbox TUI. Ensures the daemon is running first.
func runInbox() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	model := tui.NewInboxModel(client)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// runNewSession opens the inbox TUI with the new session dialog already shown.
func runNewSession() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	model := tui.NewInboxModelWithNewSession(client)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// ensureDaemon makes sure the daemon is running, starting it if needed.
// Returns a connected client.
func ensureDaemon() (*daemon.Client, error) {
	running, _, err := daemon.IsRunning()
	if err != nil {
		return nil, err
	}

	if !running {
		fmt.Println("Starting daemon...")
		if err := runDaemonStart(false); err != nil {
			return nil, fmt.Errorf("start daemon: %w", err)
		}
	}

	client, err := daemon.NewDefaultClient()
	if err != nil {
		return nil, err
	}

	// Verify reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return nil, fmt.Errorf("daemon not reachable: %w", err)
	}

	return client, nil
}
