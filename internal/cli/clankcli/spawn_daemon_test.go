package clankcli

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression for the auto-start bug where `clank` forked itself with
// `start --foreground` because the spawn helper used os.Executable().
// `clank` has no `start` subcommand, so the child died immediately and
// the user saw "daemon not reachable" instead of "clankd missing".
//
// findClankdFor must locate clankd next to clank (preferred) or on
// PATH (fallback), and return a clear error if neither is present —
// never silently fall back to the calling binary.
func TestFindClankdFor(t *testing.T) {
	t.Run("prefers sibling next to clank", func(t *testing.T) {
		dir := t.TempDir()
		clank := filepath.Join(dir, "clank")
		clankd := filepath.Join(dir, "clankd")
		writeExecutable(t, clank)
		writeExecutable(t, clankd)
		// PATH points at an unrelated dir to prove sibling wins.
		t.Setenv("PATH", t.TempDir())

		got, err := findClankdFor(clank)
		if err != nil {
			t.Fatalf("findClankdFor: %v", err)
		}
		if got != clankd {
			t.Fatalf("got %q, want %q", got, clankd)
		}
	})

	t.Run("falls back to PATH when no sibling", func(t *testing.T) {
		clank := filepath.Join(t.TempDir(), "clank")
		writeExecutable(t, clank)

		pathDir := t.TempDir()
		pathClankd := filepath.Join(pathDir, "clankd")
		writeExecutable(t, pathClankd)
		t.Setenv("PATH", pathDir)

		got, err := findClankdFor(clank)
		if err != nil {
			t.Fatalf("findClankdFor: %v", err)
		}
		if got != pathClankd {
			t.Fatalf("got %q, want %q", got, pathClankd)
		}
	})

	t.Run("errors clearly when clankd is missing", func(t *testing.T) {
		clank := filepath.Join(t.TempDir(), "clank")
		writeExecutable(t, clank)
		t.Setenv("PATH", t.TempDir())

		got, err := findClankdFor(clank)
		if err == nil {
			t.Fatalf("expected error, got %q", got)
		}
	})
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
