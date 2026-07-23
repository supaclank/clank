package acptools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Go pin constants and the embedded manifest must agree — a pin
// bump is a reviewed diff touching both.
func TestPins_MatchEmbeddedManifest(t *testing.T) {
	t.Parallel()
	raw, err := manifestFS.ReadFile("manifest/package.json")
	if err != nil {
		t.Fatalf("read embedded manifest: %v", err)
	}
	var m struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if got := m.Dependencies["@agentclientprotocol/codex-acp"]; got != PinnedCodexACPVersion {
		t.Errorf("codex-acp pin: manifest %q, const %q", got, PinnedCodexACPVersion)
	}
	if got := m.Dependencies["@openai/codex"]; got != PinnedCodexVersion {
		t.Errorf("codex pin: manifest %q, const %q", got, PinnedCodexVersion)
	}
	if got := m.Dependencies["@agentclientprotocol/claude-agent-acp"]; got != PinnedClaudeACPVersion {
		t.Errorf("claude-agent-acp pin: manifest %q, const %q", got, PinnedClaudeACPVersion)
	}
	lock, err := manifestFS.ReadFile("manifest/bun.lock")
	if err != nil || len(lock) == 0 {
		t.Fatalf("embedded bun.lock missing or empty (err=%v)", err)
	}
}

// firstMissingEntry must name whichever entry point is actually absent —
// not always CodexACPEntry — so the post-install error points at the real
// gap instead of a red herring.
func TestFirstMissingEntry_NamesTheActualGap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex-acp.js")
	claude := filepath.Join(dir, "claude-agent-acp.js")
	if err := os.WriteFile(codex, []byte("present"), 0o644); err != nil {
		t.Fatalf("write codex entry: %v", err)
	}
	p := Paths{CodexACPEntry: codex, ClaudeACPEntry: claude}

	if got := firstMissingEntry(p); got != claude {
		t.Errorf("firstMissingEntry = %q, want the missing claude entry %q", got, claude)
	}
	if entryExists(p) {
		t.Error("entryExists = true, want false with claude entry missing")
	}

	if err := os.WriteFile(claude, []byte("present"), 0o644); err != nil {
		t.Fatalf("write claude entry: %v", err)
	}
	if got := firstMissingEntry(p); got != "" {
		t.Errorf("firstMissingEntry = %q, want \"\" once both entries exist", got)
	}
	if !entryExists(p) {
		t.Error("entryExists = false, want true once both entries exist")
	}
}
