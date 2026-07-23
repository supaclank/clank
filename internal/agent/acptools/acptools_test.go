package acptools

import (
	"encoding/json"
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
	lock, err := manifestFS.ReadFile("manifest/bun.lock")
	if err != nil || len(lock) == 0 {
		t.Fatalf("embedded bun.lock missing or empty (err=%v)", err)
	}
}
