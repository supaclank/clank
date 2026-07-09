package clankcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/host"
)

func TestResolveBackend_FlagWins(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.SavePreferences(config.Preferences{DefaultBackend: "opencode"}); err != nil {
		t.Fatal(err)
	}

	got, err := resolveBackend("claude-code", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if got != agent.BackendClaudeCode {
		t.Errorf("backend: got %q, want claude-code (flag must beat preference)", got)
	}
}

func TestResolveBackend_PreferenceWhenFlagEmpty(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.SavePreferences(config.Preferences{DefaultBackend: "claude-code"}); err != nil {
		t.Fatal(err)
	}

	got, err := resolveBackend("", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if got != agent.BackendClaudeCode {
		t.Errorf("backend: got %q, want claude-code from preference", got)
	}
}

func TestResolveBackend_CorruptPreferenceWarnsAndUsesDefault(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.SavePreferences(config.Preferences{DefaultBackend: "not-a-backend"}); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	got, err := resolveBackend("", &warn)
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if got != agent.DefaultBackend {
		t.Errorf("backend: got %q, want default %q", got, agent.DefaultBackend)
	}
	if !strings.Contains(warn.String(), "warning") {
		t.Errorf("expected a warning about the corrupt preference, got %q", warn.String())
	}
}

func TestResolveBackend_InvalidFlagErrors(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	if _, err := resolveBackend("not-a-backend", &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for invalid --backend value, got nil")
	}
}

func TestNewStartRequest_EnvWorktreeID(t *testing.T) {
	t.Setenv(agent.EnvWorktreeID, "01JV7T7F9Y6XQ1R6M8R2W4K3NZ")
	dir := t.TempDir()

	req := newStartRequest(agent.BackendOpenCode, dir, "feat/x", "do the thing")
	if req.GitRef.WorktreeID != "01JV7T7F9Y6XQ1R6M8R2W4K3NZ" {
		t.Errorf("WorktreeID: got %q, want env override", req.GitRef.WorktreeID)
	}
	if req.GitRef.LocalPath != dir {
		t.Errorf("LocalPath: got %q, want %q", req.GitRef.LocalPath, dir)
	}
	if req.GitRef.WorktreeBranch != "feat/x" {
		t.Errorf("WorktreeBranch: got %q, want feat/x", req.GitRef.WorktreeBranch)
	}
	if req.Hostname != host.HostLocal {
		t.Errorf("Hostname: got %q, want %q", req.Hostname, host.HostLocal)
	}
	if req.Prompt != "do the thing" {
		t.Errorf("Prompt: got %q", req.Prompt)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// A plain directory without a stamped worktree-id must still produce a
// valid request: identity comes from LocalPath alone.
func TestNewStartRequest_NonGitDirValidatesViaLocalPath(t *testing.T) {
	t.Parallel()

	req := newStartRequest(agent.BackendOpenCode, t.TempDir(), "", "hello")
	if req.GitRef.WorktreeID != "" {
		t.Errorf("WorktreeID: got %q, want empty for a non-git dir", req.GitRef.WorktreeID)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
