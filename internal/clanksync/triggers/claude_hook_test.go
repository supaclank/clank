package triggers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallClaudeHook_PreservesExistingAndIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pre := `{
	  "hooks": {
	    "Stop": [ { "hooks": [ { "type": "command", "command": "echo user-hook" } ] } ],
	    "PostToolUse": [ { "matcher": "Bash", "hooks": [ { "type": "command", "command": "echo bash" } ] } ]
	  },
	  "model": "opus"
	}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := InstallClaudeHook("/abs/clank", dir); err != nil {
		t.Fatalf("install: %v", err)
	}
	settings := readSettings(t, dir)
	stop := stopGroups(t, settings)

	if len(stop) != 2 {
		t.Fatalf("Stop should have 2 groups (user + clank), got %d", len(stop))
	}
	if !hasCommandContaining(stop, "echo user-hook") {
		t.Error("user Stop hook was clobbered")
	}
	if !hasCommandContaining(stop, claudeHookMarker) {
		t.Error("clank Stop hook not installed")
	}
	if !hasCommandContaining(stop, "/abs/clank") {
		t.Error("clank binary path not in hook command")
	}
	if settings["model"] != "opus" {
		t.Error("unrelated 'model' key dropped")
	}
	if _, ok := settings["hooks"].(map[string]any)["PostToolUse"]; !ok {
		t.Error("PostToolUse dropped")
	}

	// Idempotent: re-install replaces, doesn't duplicate.
	if err := InstallClaudeHook("/abs/clank", dir); err != nil {
		t.Fatal(err)
	}
	if got := len(stopGroups(t, readSettings(t, dir))); got != 2 {
		t.Fatalf("re-install duplicated; Stop has %d groups", got)
	}

	// Uninstall removes only ours.
	if err := UninstallClaudeHook(dir); err != nil {
		t.Fatal(err)
	}
	stop3 := stopGroups(t, readSettings(t, dir))
	if len(stop3) != 1 || !hasCommandContaining(stop3, "echo user-hook") {
		t.Fatalf("uninstall should leave only the user hook, got %v", stop3)
	}
}

func TestInstallClaudeHook_CreatesFileWhenAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := InstallClaudeHook("/abs/clank", dir); err != nil {
		t.Fatal(err)
	}
	if !hasCommandContaining(stopGroups(t, readSettings(t, dir)), claudeHookMarker) {
		t.Error("hook not installed into fresh settings.json")
	}
}

func readSettings(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return m
}

func stopGroups(t *testing.T, settings map[string]any) []any {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	stop, _ := hooks["Stop"].([]any)
	return stop
}

func hasCommandContaining(groups []any, sub string) bool {
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		hooks, _ := gm["hooks"].([]any)
		for _, h := range hooks {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hm["command"].(string); strings.Contains(cmd, sub) {
				return true
			}
		}
	}
	return false
}
