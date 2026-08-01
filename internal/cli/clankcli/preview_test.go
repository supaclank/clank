package clankcli

import "testing"

// TestRejectPathShapedArg pins the migration guard added when `clank
// preview <file>` was removed: a single path-shaped argument must
// error instead of silently becoming an agent prompt (#207).
func TestRejectPathShapedArg(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"single file-shaped arg", []string{"README.md"}, true},
		{"single relative path", []string{"./cmd/main.go"}, true},
		{"single nested path", []string{"internal/cli/preview.go"}, true},
		{"single word prompt", []string{"summarize"}, false},
		{"no args", nil, false},
		{"multi-word prompt with a dot", []string{"fix", "the", "README.md", "typo"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectPathShapedArg(tc.args)
			if tc.wantErr && err == nil {
				t.Fatalf("rejectPathShapedArg(%v): want error, got nil", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejectPathShapedArg(%v): want no error, got %v", tc.args, err)
			}
		})
	}
}
