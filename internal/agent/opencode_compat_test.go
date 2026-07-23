package agent_test

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// The ACP floor comparator gates `opencode acp` on laptops.
func TestOpencodeVersionAtLeast(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v, floor string
		want     bool
		wantErr  bool
	}{
		{"1.17.18", "1.17.18", true, false},
		{"1.18.4", "1.17.18", true, false},
		{"2.0.0", "1.17.18", true, false},
		{"1.17.17", "1.17.18", false, false},
		{"1.15.1", "1.17.18", false, false},
		{"v1.17.18", "1.17.18", true, false},
		{"1.18.0-beta.1", "1.17.18", true, false},
		{"garbage", "1.17.18", false, true},
	}
	for _, tt := range tests {
		got, err := agent.OpencodeVersionAtLeast(tt.v, tt.floor)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s vs %s: err=%v wantErr=%v", tt.v, tt.floor, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("%s >= %s: got %v, want %v", tt.v, tt.floor, got, tt.want)
		}
	}
}
