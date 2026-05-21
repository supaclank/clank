package host_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

func TestOpenCodeBackendManagerCreateBackend(t *testing.T) {
	t.Parallel()
	// Smoke test: creating an OpenCodeBackendManager should not panic.
	mgr := host.NewOpenCodeBackendManager()
	defer mgr.Shutdown()
	_ = mgr
}

func TestClaudeBackendManagerCreateBackend(t *testing.T) {
	t.Parallel()
	mgr := host.NewClaudeBackendManager()
	defer mgr.Shutdown()

	backend, err := mgr.CreateBackend(context.Background(), agent.BackendInvocation{
		WorkDir: "/tmp/test",
	})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	if backend == nil {
		t.Fatal("expected non-nil backend")
	}

	// Open is intentionally not invoked here: it spawns the Claude
	// CLI subprocess, which would require the binary to be installed
	// in the test environment. CreateBackend succeeding (above) is
	// sufficient as a smoke test.
}

// The env-resolver closure (wired by Service.New to AuthManager) must
// be called once per CreateBackend so the latest Anthropic credential
// state is honored. Resolving stale values would let a logout silently
// linger into the next session.
func TestClaudeBackendManager_EnvResolver_ThreadsIntoExtraEnv(t *testing.T) {
	t.Parallel()
	mgr := host.NewClaudeBackendManager()
	defer mgr.Shutdown()

	calls := 0
	mgr.SetEnvResolver(func() map[string]string {
		calls++
		return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-from-resolver"}
	})

	backend, err := mgr.CreateBackend(context.Background(), agent.BackendInvocation{WorkDir: "/tmp/x"})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	if calls != 1 {
		t.Errorf("resolver call count=%d, want 1", calls)
	}
	claudeBE, ok := backend.(*agent.ClaudeCodeBackend)
	if !ok {
		t.Fatalf("backend is %T, want *agent.ClaudeCodeBackend", backend)
	}
	if got := claudeBE.ExtraEnv["CLAUDE_CODE_OAUTH_TOKEN"]; got != "tok-from-resolver" {
		t.Errorf("ExtraEnv[CLAUDE_CODE_OAUTH_TOKEN]=%q, want tok-from-resolver", got)
	}
}

// ListModels returns the three claude model families the CLI supports
// via --model. Surfaced through /models?backend=claude-code so mobile's
// compose flow can let users pick sonnet/opus/haiku. Hardcoded values
// pinned here so an SDK bump that drops one family fails loudly.
func TestClaudeBackendManager_ListModels(t *testing.T) {
	t.Parallel()
	mgr := host.NewClaudeBackendManager()
	models, err := mgr.ListModels(context.Background(), "/any/workdir")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, m := range models {
		gotIDs[m.ID] = true
		if m.ProviderName != "Anthropic" {
			t.Errorf("model %s: ProviderName=%q, want Anthropic", m.ID, m.ProviderName)
		}
		if m.ProviderID != host.ProviderAnthropicClaudeCode {
			t.Errorf("model %s: ProviderID=%q, want %s", m.ID, m.ProviderID, host.ProviderAnthropicClaudeCode)
		}
	}
	for _, want := range []string{"sonnet", "opus", "haiku"} {
		if !gotIDs[want] {
			t.Errorf("missing %s in ListModels result", want)
		}
	}
	// `inherit` is deliberately omitted — it's a passthrough, not a
	// user-pickable model.
	if gotIDs["inherit"] {
		t.Errorf("'inherit' should not be in ListModels result")
	}
}

// Without a resolver (or with a resolver that returns nil), ExtraEnv
// must remain nil — claude then falls back to its own keychain/OAuth
// login. Pins the "no credential connected = vanilla CLI behavior"
// contract.
func TestClaudeBackendManager_NoResolver_LeavesExtraEnvNil(t *testing.T) {
	t.Parallel()
	mgr := host.NewClaudeBackendManager()
	defer mgr.Shutdown()

	backend, err := mgr.CreateBackend(context.Background(), agent.BackendInvocation{WorkDir: "/tmp/x"})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	claudeBE := backend.(*agent.ClaudeCodeBackend)
	if claudeBE.ExtraEnv != nil {
		t.Errorf("ExtraEnv=%v, want nil", claudeBE.ExtraEnv)
	}

	// Nil-returning resolver behaves the same as no resolver.
	mgr.SetEnvResolver(func() map[string]string { return nil })
	backend2, err := mgr.CreateBackend(context.Background(), agent.BackendInvocation{WorkDir: "/tmp/y"})
	if err != nil {
		t.Fatalf("CreateBackend (nil-resolver): %v", err)
	}
	claudeBE2 := backend2.(*agent.ClaudeCodeBackend)
	if len(claudeBE2.ExtraEnv) != 0 {
		t.Errorf("ExtraEnv=%v, want nil/empty when resolver returns nil", claudeBE2.ExtraEnv)
	}
}

// TestClaudeBackendManagerDiscoverSessions verifies DiscoverSessions reads
// session metadata from Claude Code's on-disk JSONL transcripts under a
// CLAUDE_CONFIG_DIR-pointed temp directory and maps SDKSessionInfo into
// agent.SessionSnapshot.
func TestClaudeBackendManagerDiscoverSessions(t *testing.T) {
	// Cannot use t.Parallel because t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	seedDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, seedDir)

	writeSessionJSONL(t, projDir, "sess-A", []map[string]any{
		{"type": "queue-operation", "timestamp": "2026-04-01T00:00:00Z", "sessionId": "sess-A"},
		{
			"type":      "user",
			"uuid":      "u1",
			"timestamp": "2026-04-01T00:00:01Z",
			"sessionId": "sess-A",
			"cwd":       seedDir,
			"message":   map[string]any{"role": "user", "content": "First task"},
		},
	})
	writeSessionJSONL(t, projDir, "sess-B", []map[string]any{
		{"type": "queue-operation", "timestamp": "2026-04-02T00:00:00Z", "sessionId": "sess-B"},
		{
			"type":      "user",
			"uuid":      "u1",
			"timestamp": "2026-04-02T00:00:01Z",
			"sessionId": "sess-B",
			"cwd":       seedDir,
			"message":   map[string]any{"role": "user", "content": "Second task"},
		},
		{"type": "custom-title", "customTitle": "My Custom Title", "sessionId": "sess-B"},
	})

	mgr := host.NewClaudeBackendManager()
	defer mgr.Shutdown()

	snaps, err := mgr.DiscoverSessions(context.Background(), seedDir)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}

	byID := make(map[string]agent.SessionSnapshot, len(snaps))
	for _, s := range snaps {
		byID[s.ID] = s
	}

	a, ok := byID["sess-A"]
	if !ok {
		t.Fatal("missing sess-A in snapshots")
	}
	if a.Backend != agent.BackendClaudeCode {
		t.Errorf("sess-A.Backend = %q, want %q (manager must tag snapshots with their source backend)", a.Backend, agent.BackendClaudeCode)
	}
	if a.Title != "First task" {
		t.Errorf("sess-A.Title = %q, want %q (first prompt)", a.Title, "First task")
	}
	if a.Directory != seedDir {
		t.Errorf("sess-A.Directory = %q, want %q", a.Directory, seedDir)
	}
	if a.UpdatedAt.IsZero() {
		t.Error("sess-A.UpdatedAt is zero")
	}

	b, ok := byID["sess-B"]
	if !ok {
		t.Fatal("missing sess-B in snapshots")
	}
	if b.Title != "My Custom Title" {
		t.Errorf("sess-B.Title = %q, want %q (custom title beats first prompt)", b.Title, "My Custom Title")
	}
}

// TestClaudeBackendManagerDiscoverSessionsRequiresSeedDir documents that the
// manager refuses to discover without a seed dir (avoids accidentally
// scanning every project under CLAUDE_CONFIG_DIR).
func TestClaudeBackendManagerDiscoverSessionsRequiresSeedDir(t *testing.T) {
	t.Parallel()
	mgr := host.NewClaudeBackendManager()
	defer mgr.Shutdown()

	_, err := mgr.DiscoverSessions(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty seedDir")
	}
}

// --- Helpers (mirror those in internal/agent/claude_messages_test.go) ---

func mkClaudeProjectDir(t *testing.T, configDir, cwd string) string {
	t.Helper()
	abs, err := filepath.Abs(cwd)
	if err != nil {
		t.Fatalf("Abs(%q): %v", cwd, err)
	}
	encoded := encodeCwdLikeSDK(abs)
	dir := filepath.Join(configDir, "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// encodeCwdLikeSDK mirrors the SDK's unexported encodeCwd: every non
// alphanumeric rune becomes "-".
func encodeCwdLikeSDK(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func writeSessionJSONL(t *testing.T, dir, sessionID string, entries []map[string]any) {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode entry: %v", err)
		}
	}
}
