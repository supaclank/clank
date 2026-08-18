package clankcli

import "testing"

func TestPreviewLogDelta(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		previous string
		current  string
		want     string
	}{
		{name: "initial snapshot", current: "installing\n", want: "installing\n"},
		{name: "appended output", previous: "installing\n", current: "installing\nready\n", want: "ready\n"},
		{name: "unchanged", previous: "installing\n", current: "installing\n"},
		{name: "ring wrapped", previous: "abcdef", current: "defghi", want: "ghi"},
		{name: "ring advanced beyond overlap", previous: "abcdef", current: "uvwxyz", want: "uvwxyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(previewLogDelta([]byte(tt.previous), []byte(tt.current))); got != tt.want {
				t.Errorf("previewLogDelta(%q, %q) = %q, want %q", tt.previous, tt.current, got, tt.want)
			}
		})
	}
}
