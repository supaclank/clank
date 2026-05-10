package agent_test

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func TestParseTimeParam(t *testing.T) {
	t.Parallel()

	t.Run("relative hours", func(t *testing.T) {
		t.Parallel()
		before := time.Now()
		result, err := agent.ParseTimeParam("24h")
		after := time.Now()
		if err != nil {
			t.Fatalf("ParseTimeParam(24h): %v", err)
		}
		expectedLow := before.Add(-24 * time.Hour)
		expectedHigh := after.Add(-24 * time.Hour)
		if result.Before(expectedLow) || result.After(expectedHigh) {
			t.Errorf("24h: got %v, expected between %v and %v", result, expectedLow, expectedHigh)
		}
	})

	t.Run("relative days", func(t *testing.T) {
		t.Parallel()
		before := time.Now()
		result, err := agent.ParseTimeParam("7d")
		after := time.Now()
		if err != nil {
			t.Fatalf("ParseTimeParam(7d): %v", err)
		}
		expectedLow := before.Add(-7 * 24 * time.Hour)
		expectedHigh := after.Add(-7 * 24 * time.Hour)
		if result.Before(expectedLow) || result.After(expectedHigh) {
			t.Errorf("7d: got %v, expected between %v and %v", result, expectedLow, expectedHigh)
		}
	})

	t.Run("RFC 3339", func(t *testing.T) {
		t.Parallel()
		result, err := agent.ParseTimeParam("2026-03-15T10:30:00Z")
		if err != nil {
			t.Fatalf("ParseTimeParam(RFC3339): %v", err)
		}
		expected := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("expected %v, got %v", expected, result)
		}
	})

	t.Run("invalid inputs", func(t *testing.T) {
		t.Parallel()
		for _, input := range []string{"", "x", "abc", "7x", "0d", "-3d"} {
			_, err := agent.ParseTimeParam(input)
			if err == nil {
				t.Errorf("expected error for %q, got nil", input)
			}
		}
	})
}

// §7.3: GitRef is the sole repo-identity field on StartRequest. Validate
// must accept remote and local refs, reject when missing, and propagate
// GitRef.Validate failures.
func TestStartRequest_Validate_GitRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		req     agent.StartRequest
		wantErr bool
	}{
		{
			name: "git_ref_remote_ok",
			req: agent.StartRequest{
				Backend: agent.BackendOpenCode,
				GitRef:  agent.GitRef{WorktreeID: "01HXYZWORKTREE"},
				Prompt:  "hi",
			},
		},
		{
			name: "git_ref_local_ok",
			req: agent.StartRequest{
				Backend: agent.BackendClaudeCode,
				GitRef:  agent.GitRef{LocalPath: "/tmp/repo"},
				Prompt:  "hi",
			},
		},
		{
			name: "git_ref_missing_rejected",
			req: agent.StartRequest{
				Backend: agent.BackendOpenCode,
				Prompt:  "hi",
			},
			wantErr: true,
		},
		{
			name: "git_ref_invalid_propagates",
			req: agent.StartRequest{
				Backend: agent.BackendOpenCode,
				GitRef:  agent.GitRef{LocalPath: "rel"}, // present but invalid: not absolute
				Prompt:  "hi",
			},
			wantErr: true,
		},
		{
			name: "git_ref_both_set_allowed",
			req: agent.StartRequest{
				Backend: agent.BackendOpenCode,
				GitRef: agent.GitRef{
					LocalPath: "/tmp/repo",
					WorktreeID: "01HXYZWORKTREE",
				},
				Prompt: "hi",
			},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.req.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
