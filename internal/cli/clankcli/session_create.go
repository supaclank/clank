package clankcli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/host"
)

// resolveProjectDir defaults to the current working directory and resolves
// to an absolute path so GitRef.LocalPath is stable regardless of where the
// daemon happens to be running from when it consumes the request.
func resolveProjectDir(projectDir string) (string, error) {
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		projectDir = cwd
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve project dir %q: %w", projectDir, err)
	}
	return abs, nil
}

// resolveBackend resolves the backend type. Precedence:
//  1. flag value (explicit override)
//  2. preferences.json default_backend
//  3. agent.DefaultBackend
//
// A corrupt preference warns to warnOut and proceeds with the default.
func resolveBackend(flagValue string, warnOut io.Writer) (agent.BackendType, error) {
	if flagValue != "" {
		return agent.ParseBackend(flagValue)
	}
	prefs, _ := config.LoadPreferences()
	resolved, err := agent.ResolveBackendPreference(prefs.DefaultBackend)
	if err != nil {
		fmt.Fprintf(warnOut, "warning: %v; using %s\n", err, resolved)
	}
	return resolved, nil
}

// newStartRequest builds the wire request for a laptop-local session in
// projectDir carrying the initial prompt.
func newStartRequest(backend agent.BackendType, projectDir, worktreeBranch, prompt string) agent.StartRequest {
	worktreeID, _ := agent.ReadLocalWorktreeID(projectDir) // empty for plain local repos without a stamped worktree-id
	return agent.StartRequest{
		Backend:  backend,
		Hostname: host.HostLocal,
		GitRef: agent.GitRef{
			LocalPath:      projectDir,
			WorktreeID:     worktreeID,
			WorktreeBranch: worktreeBranch,
		},
		Prompt: prompt,
	}
}
