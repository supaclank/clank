package agent_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

func TestAssertOpencodeVersionsCompatible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		local, remote     string
		wantWarn          bool
		wantErr           bool
		errContainsAny    []string // any of these substrings should be in the error
	}{
		{
			name:   "exact match",
			local:  "1.14.48",
			remote: "1.14.48",
		},
		{
			name:     "patch differs — warn but allow",
			local:    "1.14.48",
			remote:   "1.14.49",
			wantWarn: true,
		},
		{
			name:     "patch differs reverse",
			local:    "1.14.49",
			remote:   "1.14.48",
			wantWarn: true,
		},
		{
			name:           "minor differs — refuse (the production case)",
			local:          "1.3.15",
			remote:         "1.14.48",
			wantErr:        true,
			errContainsAny: []string{"minor version differs"},
		},
		{
			name:           "major differs — refuse",
			local:          "1.14.48",
			remote:         "2.0.0",
			wantErr:        true,
			errContainsAny: []string{"major version differs"},
		},
		{
			name:           "empty local — refuse",
			local:          "",
			remote:         "1.14.48",
			wantErr:        true,
			errContainsAny: []string{"could not determine"},
		},
		{
			name:           "empty remote — refuse",
			local:          "1.14.48",
			remote:         "",
			wantErr:        true,
			errContainsAny: []string{"could not determine"},
		},
		{
			name:           "garbage local — refuse with parse error",
			local:          "not-a-version",
			remote:         "1.14.48",
			wantErr:        true,
			errContainsAny: []string{"unparseable"},
		},
		{
			name:   "two-segment vs three-segment: semantically equal, no warning",
			local:  "1.14",
			remote: "1.14.0",
			// 1.14 → (1,14,0); 1.14.0 → (1,14,0). Numerically equal
			// after parse — no warning.
		},
		{
			name:           "both garbage and equal — must still refuse via parse error",
			local:          "garbage",
			remote:         "garbage",
			wantErr:        true,
			errContainsAny: []string{"unparseable"},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			warn, err := agent.AssertOpencodeVersionsCompatible(c.local, c.remote)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got warn=%v", warn)
				}
				var typed *agent.OpencodeIncompatibleError
				if !errors.As(err, &typed) {
					t.Errorf("error should be *OpencodeIncompatibleError, got %T", err)
				}
				for _, want := range c.errContainsAny {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q missing expected substring %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantWarn && warn == nil {
				t.Errorf("expected a warning but got nil")
			}
			if !c.wantWarn && warn != nil {
				t.Errorf("expected no warning but got: %v", warn)
			}
		})
	}
}
