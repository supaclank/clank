package clankcli

import (
	"context"
	"fmt"
	"io"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/tui"
)

// runConnect boots the daemon if needed and hands the terminal to the
// connect UI. backend, when set, skips the backend picker.
func runConnect(ctx context.Context, backend agent.BackendType, in io.Reader, out io.Writer) error {
	if !isInteractiveTerminal(in, out) {
		return errConnectNeedsTTY
	}
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	result, err := showConnectUI(ctx, client, backend, in, out)
	if err != nil {
		return err
	}
	reportConnectResult(result, out)
	return nil
}

// showConnectUI runs the connect program against the local host and
// persists the connected backend as the default when the user has no
// preference yet — a first connect should be the backend their next
// session actually uses, not whatever agent.DefaultBackend happens to
// be. An existing preference is never overwritten.
func showConnectUI(ctx context.Context, client *daemonclient.Client, backend agent.BackendType, in io.Reader, out io.Writer) (tui.ConnectResult, error) {
	tui.ApplyPreferredTheme()
	model := tui.NewConnectModel(client.Host(host.HostLocal), backend)

	cleanupLogs := redirectLogToFile()
	defer cleanupLogs()

	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	if _, err := program.Run(); err != nil {
		return tui.ConnectResult{}, fmt.Errorf("run connect: %w", err)
	}

	result := model.Result()
	if result.IsConnected {
		if err := adoptDefaultBackend(result.Backend, out); err != nil {
			return result, err
		}
	}
	return result, nil
}

// adoptDefaultBackend records backend as the default for future sessions
// when preferences.json carries no choice yet.
func adoptDefaultBackend(backend agent.BackendType, out io.Writer) error {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return fmt.Errorf("read preferences: %w", err)
	}
	if prefs.DefaultBackend != "" {
		return nil
	}
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.DefaultBackend = string(backend)
	}); err != nil {
		return fmt.Errorf("save default backend: %w", err)
	}
	fmt.Fprintf(out, "%s is now your default agent (change it in the inbox's settings).\n", backend)
	return nil
}

// reportConnectResult prints the one line the user needs after the UI
// tears itself down. A canceled run is not an error — they said no.
func reportConnectResult(result tui.ConnectResult, out io.Writer) {
	if result.IsConnected {
		fmt.Fprintf(out, "Connected %s.\n", result.Backend)
		return
	}
	fmt.Fprintf(out, "Nothing connected — %s.\n", connectHint)
}
