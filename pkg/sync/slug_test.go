package sync

import (
	"strings"
	"testing"
)

func TestSlugifyWorktreeID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"clank", "clank"},
		{"Clank", "clank"},
		{"My Cool Repo", "my-cool-repo"},
		{"  spaced  ", "spaced"},
		{"slash/in/name", "slash-in-name"},
		{"with.dots.and_underscores", "with.dots.and_underscores"},
		{"unicode-ümläut", "unicode-ml-ut"}, // non-ASCII collapses to dash; consecutive dashes squashed
		{"---dashes---", "dashes"},
		{"123-numbers", "123-numbers"},
		{"!!!", "worktree"},      // all punctuation → fallback
		{"", "worktree"},          // empty → fallback
		{"a/b/c", "a-b-c"},
		{"multi   spaces", "multi-spaces"}, // runs collapse to single dash
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := slugifyWorktreeID(tc.in)
			if got != tc.want {
				t.Errorf("slugifyWorktreeID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCapWorktreeIDBase(t *testing.T) {
	t.Parallel()
	if got := capWorktreeIDBase("short"); got != "short" {
		t.Errorf("under-cap input changed: got %q", got)
	}

	long := strings.Repeat("a", maxWorktreeIDBase+10)
	got := capWorktreeIDBase(long)
	if len(got) != maxWorktreeIDBase {
		t.Errorf("cap to %d, got len %d", maxWorktreeIDBase, len(got))
	}

	// Truncation at a separator boundary trims it so the result is a clean slug.
	with := strings.Repeat("a", maxWorktreeIDBase-1) + "-extra"
	if g := capWorktreeIDBase(with); strings.HasSuffix(g, "-") {
		t.Errorf("trailing separator not trimmed: %q", g)
	}
}
