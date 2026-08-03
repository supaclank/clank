package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
)

func TestPartitionWorktreesByActivityWindow_AllRecentStayVisible(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 21, 12, 0, 0, 0, time.Local)
	in := []worktreeNode{
		{LocalPath: "/r/a", LatestUpdatedAt: base},
		{LocalPath: "/r/b", LatestUpdatedAt: base.Add(-2 * time.Hour)},
		{LocalPath: "/r/c", LatestUpdatedAt: base.AddDate(0, 0, -3)},
	}
	recent, older := PartitionWorktreesByActivityWindow(in, "")
	if got := pathsOf(recent); !equalStrings(got, []string{"/r/a", "/r/b", "/r/c"}) {
		t.Errorf("recent: got %v", got)
	}
	if len(older) != 0 {
		t.Errorf("older: expected empty within window, got %v", pathsOf(older))
	}
}

func TestPartitionWorktreesByActivityWindow_BeyondCutoffHides(t *testing.T) {
	t.Parallel()
	latest := time.Date(2026, 5, 21, 14, 0, 0, 0, time.Local)
	in := []worktreeNode{
		{LocalPath: "/r/today", LatestUpdatedAt: latest},
		{LocalPath: "/r/lastweek", LatestUpdatedAt: latest.AddDate(0, 0, -RecentWindowDays).Add(time.Hour)}, // inside the window
		{LocalPath: "/r/oneweek", LatestUpdatedAt: latest.AddDate(0, 0, -RecentWindowDays)},                 // exactly on the boundary (kept)
		{LocalPath: "/r/older", LatestUpdatedAt: latest.AddDate(0, 0, -RecentWindowDays-1)},                 // past the boundary
	}
	recent, older := PartitionWorktreesByActivityWindow(in, "")
	if got := pathsOf(recent); !equalStrings(got, []string{"/r/today", "/r/lastweek", "/r/oneweek"}) {
		t.Errorf("recent: got %v", got)
	}
	if got := pathsOf(older); !equalStrings(got, []string{"/r/older"}) {
		t.Errorf("older: got %v", got)
	}
}

func TestPartitionWorktreesByActivityWindow_AnchorsOnLatestNotNow(t *testing.T) {
	t.Parallel()
	// If everyone was last touched 30 days ago, those still stay visible
	// — the window slides with whatever the "latest" actually is.
	latest := time.Date(2026, 4, 21, 10, 0, 0, 0, time.Local)
	in := []worktreeNode{
		{LocalPath: "/r/a", LatestUpdatedAt: latest},
		{LocalPath: "/r/b", LatestUpdatedAt: latest.AddDate(0, 0, -3)},
		{LocalPath: "/r/old", LatestUpdatedAt: latest.AddDate(0, 0, -30)},
	}
	recent, older := PartitionWorktreesByActivityWindow(in, "")
	if got := pathsOf(recent); !equalStrings(got, []string{"/r/a", "/r/b"}) {
		t.Errorf("recent: got %v", got)
	}
	if got := pathsOf(older); !equalStrings(got, []string{"/r/old"}) {
		t.Errorf("older: got %v", got)
	}
}

func TestPartitionWorktreesByActivityWindow_CwdAlwaysVisible(t *testing.T) {
	t.Parallel()
	// The cwd worktree should stay visible even when its last activity
	// is well past the recent window — you opened the TUI there, so it
	// matters regardless of age.
	latest := time.Date(2026, 5, 21, 14, 0, 0, 0, time.Local)
	in := []worktreeNode{
		{LocalPath: "/r/active", LatestUpdatedAt: latest},
		{LocalPath: "/r/cwd", LatestUpdatedAt: latest.AddDate(0, 0, -90)},
		{LocalPath: "/r/random", LatestUpdatedAt: latest.AddDate(0, 0, -90)},
	}
	recent, older := PartitionWorktreesByActivityWindow(in, "/r/cwd")
	if got := pathsOf(recent); !equalStrings(got, []string{"/r/active", "/r/cwd"}) {
		t.Errorf("recent: got %v, want [/r/active /r/cwd]", got)
	}
	if got := pathsOf(older); !equalStrings(got, []string{"/r/random"}) {
		t.Errorf("older: got %v, want [/r/random]", got)
	}
}

func TestPartitionWorktreesByActivityWindow_EmptyInput(t *testing.T) {
	t.Parallel()
	recent, older := PartitionWorktreesByActivityWindow(nil, "/some/cwd")
	if recent != nil || older != nil {
		t.Errorf("empty input should return nil slices, got recent=%v older=%v", recent, older)
	}
}

func TestPartitionSessionsByRecency_UnderLimit(t *testing.T) {
	t.Parallel()
	in := make([]agent.SessionInfo, MaxRecentSessionsBeforeBucket)
	for i := range in {
		in[i] = agent.SessionInfo{ID: fmt.Sprintf("s%d", i)}
	}
	recent, older := PartitionSessionsByRecency(in)
	if len(recent) != MaxRecentSessionsBeforeBucket {
		t.Errorf("recent: expected %d, got %d", MaxRecentSessionsBeforeBucket, len(recent))
	}
	if len(older) != 0 {
		t.Errorf("older: expected empty at exact limit, got %d", len(older))
	}
}

func TestPartitionSessionsByRecency_OverflowGoesToOlder(t *testing.T) {
	t.Parallel()
	total := MaxRecentSessionsBeforeBucket + 3
	in := make([]agent.SessionInfo, total)
	for i := range in {
		in[i] = agent.SessionInfo{ID: fmt.Sprintf("s%d", i), UpdatedAt: time.Now().Add(-time.Duration(i) * time.Hour)}
	}
	recent, older := PartitionSessionsByRecency(in)
	if len(recent) != MaxRecentSessionsBeforeBucket {
		t.Errorf("recent: expected %d, got %d", MaxRecentSessionsBeforeBucket, len(recent))
	}
	if len(older) != total-MaxRecentSessionsBeforeBucket {
		t.Errorf("older: expected %d, got %d", total-MaxRecentSessionsBeforeBucket, len(older))
	}
	if recent[0].ID != "s0" {
		t.Errorf("first recent should be s0, got %s", recent[0].ID)
	}
	if older[0].ID != fmt.Sprintf("s%d", MaxRecentSessionsBeforeBucket) {
		t.Errorf("first older should be the first overflow, got %s", older[0].ID)
	}
}

func TestPartitionSessionsByRecency_OutputsAreCopies(t *testing.T) {
	t.Parallel()
	in := []agent.SessionInfo{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	recent, _ := PartitionSessionsByRecency(in)
	recent[0].ID = "MUTATED"
	if in[0].ID == "MUTATED" {
		t.Errorf("expected partition output to be an independent copy")
	}
}

func pathsOf(in []worktreeNode) []string {
	out := make([]string, len(in))
	for i, w := range in {
		out[i] = w.LocalPath
	}
	return out
}

func idsOf(in []agent.SessionInfo) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.ID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
