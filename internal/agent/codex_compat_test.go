package agent

import "testing"

func TestParseCodexVersionOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"current shape", "codex-cli 0.144.6\n", "0.144.6"},
		{"no trailing newline", "codex-cli 0.144.6", "0.144.6"},
		{"unknown prefix", "codex 0.144.6", ""},
		{"empty", "", ""},
		{"extra fields", "codex-cli 0.144.6 (release)", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseCodexVersionOutput(tc.in); got != tc.want {
				t.Errorf("parseCodexVersionOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
