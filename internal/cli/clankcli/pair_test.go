package clankcli

import (
	"bytes"
	"os"
	"testing"
)

// TestStdinIsTTY_DevNullIsNotATerminal pins the bug fixed by switching to
// term.IsTerminal: os.ModeCharDevice alone is also set on /dev/null, so a
// naive check misidentified redirected-from-/dev/null scripts as interactive.
func TestStdinIsTTY_DevNullIsNotATerminal(t *testing.T) {
	t.Parallel()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer f.Close()

	if stdinIsTTY(f) {
		t.Fatalf("stdinIsTTY(%s) = true, want false", os.DevNull)
	}
}

// TestStdinIsTTY_NonFileReaderIsNotATerminal covers the type-assert guard
// for readers that aren't *os.File at all (e.g. bytes.Reader in tests).
func TestStdinIsTTY_NonFileReaderIsNotATerminal(t *testing.T) {
	t.Parallel()
	if stdinIsTTY(bytes.NewReader(nil)) {
		t.Fatalf("stdinIsTTY(bytes.Reader) = true, want false")
	}
}

func TestShortHostnameDropsDomainSuffix(t *testing.T) {
	t.Parallel()
	// shortHostname reads os.Hostname; pin the suffix-stripping rule via
	// the same Cut logic on representative values.
	cases := map[string]string{
		"Axels-MacBook-Pro.local": "Axels-MacBook-Pro",
		"Mac.lan":                 "Mac",
		"plainhost":               "plainhost",
	}
	for in, want := range cases {
		if got := stripHostSuffix(in); got != want {
			t.Errorf("stripHostSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
