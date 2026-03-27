// Package clankcli provides the root cobra command for the clank binary.
package clankcli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/cli/daemoncli"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/daemon"
	"github.com/acksell/clank/internal/tui"
)

// Command returns the root cobra command for the clank binary with all subcommands.
func Command() *cobra.Command {
	root := &cobra.Command{
		Use:   "clank",
		Short: "AI-powered coding session manager",
		Long:  "Clank manages your coding agent sessions and helps you track what's in flight.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInbox()
		},
	}

	root.AddCommand(
		configCmd(),
		codeCmd(),
		inboxCmd(),
	)

	return root
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

			// Step 2: API key + model.
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
			default:
				return fmt.Errorf("unknown config key: %s\nValid keys: llm.provider, llm.api_key, llm.model, llm.base_url", args[0])
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
				// No prompt — open composing view standalone.
				return runComposing(projectDir)
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
			p := tea.NewProgram(model)
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
	p := tea.NewProgram(model)
	_, err = p.Run()
	return err
}

// runComposing opens the composing view standalone (not inside inbox).
// The user types their first prompt and the session is created on send.
func runComposing(projectDir string) error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		projectDir = cwd
	}

	model := tui.NewSessionViewComposing(client, projectDir)
	model.SetStandalone(true)
	p := tea.NewProgram(model)
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
		if err := daemoncli.RunStart(false); err != nil {
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
