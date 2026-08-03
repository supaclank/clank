package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/agent"
)

// TestPrintPins pins the shell-sourceable contract the host-image build
// depends on: exactly KEY=VALUE lines, values equal to the agent
// constants (so a bumped pin flows into the image), no spaces that
// would break `. pins.env`.
func TestPrintPins(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := printPins(&buf); err != nil {
		t.Fatalf("printPins: %v", err)
	}

	want := map[string]string{
		"CLAUDE_VERSION":   agent.PinnedClaudeVersion,
		"OPENCODE_VERSION": agent.PinnedOpencodeVersion,
		"BUN_VERSION":      agent.PinnedBunVersion,
	}
	got := map[string]string{}
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("line %q is not KEY=VALUE", line)
		}
		if strings.ContainsAny(line, " \t") {
			t.Errorf("line %q has whitespace — breaks `. pins.env`", line)
		}
		if v == "" {
			t.Errorf("key %q has empty value", k)
		}
		got[k] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("emitted %d pins, want %d (%v)", len(got), len(want), got)
	}
}
