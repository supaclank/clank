package clankcli

import (
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

func TestFormatOpencodeIncompatibleHint(t *testing.T) {
	t.Parallel()
	const pin = "1.14.49"
	cases := []struct {
		name           string
		local, remote  string
		mustContain    []string
		mustNotContain []string
	}{
		{
			// Regression: a user reported
			//
			//   Error: opencode version mismatch (local=1.15.1, remote=1.14.49)
			//   ...
			//   clank pins opencode at version 1.14.49. Upgrade your laptop to match:
			//     opencode upgrade v1.14.49
			//
			// when the laptop was actually ahead of clank's pin (it
			// was clank that was behind, not the laptop). The hint
			// must call out that clank is the likely lagging side
			// and offer "update clank" before "downgrade laptop" —
			// never just "Upgrade your laptop to match".
			name:   "laptop ahead of pin — points at clank, not the laptop",
			local:  "1.15.1",
			remote: pin,
			mustContain: []string{
				"1.15.1",
				"newer",
				"clank itself is behind",
				"update clank",
			},
			mustNotContain: []string{
				"Upgrade your laptop to match",
			},
		},
		{
			name:   "laptop behind pin — tells user to upgrade laptop",
			local:  "1.3.15",
			remote: pin,
			mustContain: []string{
				"1.3.15",
				"Upgrade your laptop to match",
				"opencode upgrade v" + pin,
			},
			mustNotContain: []string{
				"clank itself is behind",
				"newer",
			},
		},
		{
			name:   "sprite drifted — tells user to restart the sprite",
			local:  pin,
			remote: "1.13.0",
			mustContain: []string{
				"1.13.0",
				"Restart the sprite",
				"EnsureHost",
			},
			mustNotContain: []string{
				"Upgrade your laptop to match",
				"clank itself is behind",
			},
		},
		{
			name:   "both drifted — surfaces all three versions",
			local:  "1.15.0",
			remote: "1.13.0",
			mustContain: []string{
				"1.15.0",
				"1.13.0",
				pin,
				"neither side matches",
			},
		},
		{
			name:   "unparseable side — falls back to generic hint",
			local:  "garbage",
			remote: pin,
			mustContain: []string{
				pin,
				"Bring both sides to the pin",
			},
			mustNotContain: []string{
				"Upgrade your laptop to match",
				"Restart the sprite",
				"clank itself is behind",
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := &agent.OpencodeIncompatibleError{
				Local: c.local, Remote: c.remote,
				Reason: "minor version differs",
			}
			got := formatOpencodeIncompatibleHint(e, pin).Error()
			if !strings.Contains(got, e.Error()) {
				t.Errorf("hint must include the original error message %q; got %q", e.Error(), got)
			}
			for _, want := range c.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("hint missing %q\nfull hint:\n%s", want, got)
				}
			}
			for _, bad := range c.mustNotContain {
				if strings.Contains(got, bad) {
					t.Errorf("hint must not contain %q\nfull hint:\n%s", bad, got)
				}
			}
		})
	}
}
