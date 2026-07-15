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
		name           string
		local, remote  string
		wantWarn       bool
		wantErr        bool
		errContainsAny []string // any of these substrings should be in the error
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

func TestDiagnoseOpencodeMismatch(t *testing.T) {
	t.Parallel()
	const pin = "1.14.49"
	cases := []struct {
		name          string
		local, remote string
		want          agent.OpencodeMismatch
	}{
		{
			// The reported bug: laptop on 1.15.1, sprite reinstalled
			// to the pin (1.14.49). The old message said "Upgrade
			// your laptop to match: opencode upgrade v1.14.49",
			// suggesting a downgrade when the laptop is the newer
			// side. The classifier must mark this as LaptopAhead so
			// callers can phrase advice around "clank's pin may be
			// stale" instead.
			name:   "laptop ahead of pin, sprite at pin",
			local:  "1.15.1",
			remote: pin,
			want:   agent.OpencodeMismatchLaptopAheadOfPin,
		},
		{
			name:   "laptop behind pin, sprite at pin",
			local:  "1.3.15",
			remote: pin,
			want:   agent.OpencodeMismatchLaptopBehindPin,
		},
		{
			name:   "sprite drifted, laptop at pin",
			local:  pin,
			remote: "1.13.0",
			want:   agent.OpencodeMismatchSpriteDrifted,
		},
		{
			name:   "both drifted from pin in different directions",
			local:  "1.15.0",
			remote: "1.13.0",
			want:   agent.OpencodeMismatchBothDrifted,
		},
		{
			name:   "both drifted to same older minor",
			local:  "1.13.0",
			remote: "1.13.0",
			want:   agent.OpencodeMismatchBothDrifted,
		},
		{
			name:   "unparseable local",
			local:  "garbage",
			remote: pin,
			want:   agent.OpencodeMismatchUnknown,
		},
		{
			name:   "unparseable remote",
			local:  pin,
			remote: "",
			want:   agent.OpencodeMismatchUnknown,
		},
		{
			name:   "two-segment laptop equals three-segment pin",
			local:  "1.14",
			remote: pin,
			// 1.14 → (1,14,0) and pin 1.14.49 → (1,14,49), so laptop
			// is behind the pin by patch — still classified as
			// LaptopBehindPin.
			want: agent.OpencodeMismatchLaptopBehindPin,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := agent.DiagnoseOpencodeMismatch(c.local, c.remote, pin)
			if got != c.want {
				t.Errorf("DiagnoseOpencodeMismatch(%q, %q, %q) = %d; want %d",
					c.local, c.remote, pin, got, c.want)
			}
		})
	}
}
