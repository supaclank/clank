package host

import "testing"

func TestRepoLabelFromURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		fallback string
		want     string
	}{
		{
			name:  "https with .git",
			input: "https://github.com/acme/api.git",
			want:  "acme/api",
		},
		{
			name:  "scp-style",
			input: "git@github.com:acme/api.git",
			want:  "acme/api",
		},
		{
			name:  "https without .git",
			input: "https://github.com/acme/api",
			want:  "acme/api",
		},
		{
			name:  "ssh url",
			input: "ssh://git@github.com/acme/api.git",
			want:  "acme/api",
		},
		{
			name:  "single-segment path",
			input: "https://example.com/api",
			want:  "api",
		},
		{
			name:  "trailing slash",
			input: "https://github.com/acme/api/",
			want:  "acme/api",
		},
		{
			name:     "empty url falls back",
			input:    "",
			fallback: "fb",
			want:     "fb",
		},
		{
			name:     "no path falls back",
			input:    "https://example.com/",
			fallback: "fb",
			want:     "fb",
		},
		{
			name:  "deep path keeps last two",
			input: "https://gitlab.com/group/sub/api.git",
			want:  "sub/api",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := repoLabelFromURL(tc.input, tc.fallback)
			if got != tc.want {
				t.Errorf("repoLabelFromURL(%q, %q) = %q, want %q", tc.input, tc.fallback, got, tc.want)
			}
		})
	}
}

// Regression: forks with the same repo name must produce distinct labels.
func TestRepoLabelFromURL_ForksAreDistinct(t *testing.T) {
	t.Parallel()

	a := repoLabelFromURL("https://github.com/acme/api.git", "")
	b := repoLabelFromURL("https://github.com/bob/api.git", "")
	if a == b {
		t.Fatalf("expected distinct labels for forks, got %q == %q", a, b)
	}
	if a != "acme/api" || b != "bob/api" {
		t.Fatalf("unexpected labels: a=%q b=%q", a, b)
	}
}
