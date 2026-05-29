// Package triggers installs the agent-lifecycle hooks that fire
// `clank push` on idle: a Claude Code Stop hook and an opencode
// session.idle plugin. They are installed globally (user config) once
// and stay inert until a repo is tracked — `clank push` fast-exits for
// untracked worktrees, so the global install is safe.
package triggers

import (
	"fmt"
	"os"
	"path/filepath"
)

// Install writes both triggers, pointed at the currently-running clank
// binary. Idempotent.
func Install() error {
	clankBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve clank binary: %w", err)
	}
	cdir, err := claudeDir()
	if err != nil {
		return err
	}
	if err := InstallClaudeHook(clankBin, cdir); err != nil {
		return fmt.Errorf("install claude hook: %w", err)
	}
	ocdir, err := opencodeConfigDir()
	if err != nil {
		return err
	}
	if err := InstallOpenCodePlugin(clankBin, ocdir); err != nil {
		return fmt.Errorf("install opencode plugin: %w", err)
	}
	return nil
}

// Uninstall removes both triggers. Idempotent.
func Uninstall() error {
	cdir, err := claudeDir()
	if err != nil {
		return err
	}
	if err := UninstallClaudeHook(cdir); err != nil {
		return fmt.Errorf("uninstall claude hook: %w", err)
	}
	ocdir, err := opencodeConfigDir()
	if err != nil {
		return err
	}
	if err := UninstallOpenCodePlugin(ocdir); err != nil {
		return fmt.Errorf("uninstall opencode plugin: %w", err)
	}
	return nil
}

// claudeDir resolves Claude Code's user config dir, honoring
// CLAUDE_CONFIG_DIR and falling back to ~/.claude.
func claudeDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// opencodeConfigDir resolves opencode's user config dir, honoring
// XDG_CONFIG_HOME and falling back to ~/.config/opencode.
func opencodeConfigDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}
