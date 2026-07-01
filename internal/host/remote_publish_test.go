package host

import (
	"strings"
	"testing"
)

func TestSanitizeRepoName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"My Todo App", "My-Todo-App"},
		{"hello", "hello"},
		{"a b  c", "a-b-c"},      // runs of separators collapse to one dash
		{"  spaced  ", "spaced"}, // leading/trailing separators trimmed
		{"weird!!!name", "weird-name"},
		{"under_score.dot-dash", "under_score.dot-dash"}, // . _ - are all allowed
		{"---trim---", "trim"},
		{"", ""},
		{"!!!", ""}, // nothing usable remains
		{"café münchen", "caf-m-nchen"},
	}
	for _, c := range cases {
		if got := sanitizeRepoName(c.in); got != c.want {
			t.Errorf("sanitizeRepoName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeRepoName_CapsLength(t *testing.T) {
	t.Parallel()
	got := sanitizeRepoName(strings.Repeat("a", maxRepoNameLength+50))
	if len(got) != maxRepoNameLength {
		t.Errorf("len(sanitizeRepoName(...)) = %d, want %d", len(got), maxRepoNameLength)
	}

	// A cut that lands right after a separator must trim it, not leave a
	// trailing dash that github.com/.../create_repo.go would then reject.
	got = sanitizeRepoName(strings.Repeat("a", maxRepoNameLength-1) + "---suffix")
	if strings.HasSuffix(got, "-") {
		t.Errorf("sanitizeRepoName(...) = %q, want no trailing separator after truncation", got)
	}
}
