package clankcli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/agent/presets"
	"github.com/supaclank/clank/internal/config"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
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

// defaultPresetConfig fetches the host's presets and returns a copy of
// the backend's built-in Default ("Build") config — the bundle headless
// creates apply verbatim, which by construction carries every create-time
// required key (the host 400s a create missing any and never fills values
// in). No fallbacks: a host without the preset errors here by name.
func defaultPresetConfig(ctx context.Context, client *daemonclient.Client, bt agent.BackendType, hostname string) (map[string]string, error) {
	ps, err := client.Backend(bt).Presets(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("fetch %s presets: %w", bt, err)
	}
	p := presets.DefaultFor(ps, bt)
	if p == nil {
		return nil, fmt.Errorf("host serves no built-in Default preset for backend %s (is the host's $CLANK_BUILTIN_PRESETS misdeclared?)", bt)
	}
	return maps.Clone(p.Config), nil
}
