package host

import "testing"

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
