package hosttest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/agent/acptools"
	"github.com/supaclank/clank/internal/host"
)

// NewCodexDiscoveryManager starts the pinned adapter over isolated local rollouts.
// It requires CLANK_TEST_CODEX_ACP=1 and never sends a model prompt.
func NewCodexDiscoveryManager(t *testing.T, sessions []agent.SessionSnapshot) *host.ACPBackendManager {
	t.Helper()
	if os.Getenv("CLANK_TEST_CODEX_ACP") == "" {
		t.Skip("set CLANK_TEST_CODEX_ACP=1 to run real Codex discovery")
	}
	codexHome := t.TempDir()
	for _, session := range sessions {
		writeCodexRollout(t, codexHome, session)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	dirs := host.ACPDirs{Tools: t.TempDir()}
	paths, err := acptools.Ensure(ctx, dirs.Tools)
	if err != nil {
		t.Fatalf("provision Codex adapter: %v", err)
	}
	// Host startup rewires adapter env resolvers, so isolate the host's environment too.
	t.Setenv("HOME", t.TempDir())
	t.Setenv(host.EnvCodexHome, codexHome)
	t.Setenv(host.EnvCodexAPIKey, "")
	t.Setenv("OPENAI_API_KEY", "")
	// Select API-key mode without borrowing credentials; local discovery needs no valid key.
	login := exec.CommandContext(ctx, paths.BunBin, paths.CodexBin, "login", "--with-api-key")
	login.Stdin = strings.NewReader("unused-for-local-session-discovery")
	if out, err := login.CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated Codex login: %v: %s", err, out)
	}
	mgr, err := host.NewCodexACPManager(dirs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	return mgr
}

func writeCodexRollout(t *testing.T, codexHome string, session agent.SessionSnapshot) {
	t.Helper()
	stamp := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	for _, row := range []map[string]any{
		{"timestamp": stamp, "type": "session_meta", "payload": map[string]any{
			"id": session.ID, "timestamp": stamp, "cwd": session.Directory,
			"originator": "codex_vscode", "cli_version": acptools.PinnedCodexVersion,
			"source": "vscode", "model_provider": "openai",
		}},
		{"timestamp": stamp, "type": "event_msg", "payload": map[string]any{
			"type": "user_message", "message": session.Title, "images": []string{}, "local_images": []string{},
		}},
	} {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(codexHome, "sessions", session.UpdatedAt.UTC().Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("rollout-%s-%s.jsonl", session.UpdatedAt.UTC().Format("2006-01-02T15-04-05"), session.ID)
	if err := os.WriteFile(filepath.Join(dir, name), data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
