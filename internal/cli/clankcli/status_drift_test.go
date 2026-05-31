package clankcli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// TestClassifyDrift pins the local-only ahead/behind/diverged
// classification across a small commit graph.
func TestClassifyDrift(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := newGitRepo(t) // commit c1
	c1 := pRev(t, repo, "HEAD")
	pWrite(t, filepath.Join(repo, "f.txt"), "v2")
	pgit(t, repo, "add", ".")
	pgit(t, repo, "commit", "-qm", "c2")
	c2 := pRev(t, repo, "HEAD")

	cases := []struct {
		name          string
		local, remote string
		want          driftState
		wantAhead     int
		wantBehind    int
	}{
		{"local ahead of remote", c2, c1, driftAhead, 1, 0},
		{"local behind remote", c1, c2, driftBehind, 0, 1},
		{"same head (uncommitted drift)", c2, c2, driftAhead, 0, 0},
		{"remote head unknown locally", c2, "1234567890123456789012345678901234567890", driftBehind, 0, 0},
	}
	for _, tc := range cases {
		got := classifyDrift(repo, tc.local, tc.remote)
		if got.state != tc.want || got.ahead != tc.wantAhead || got.behind != tc.wantBehind {
			t.Errorf("%s: classifyDrift = %+v, want {state:%d ahead:%d behind:%d}",
				tc.name, got, tc.want, tc.wantAhead, tc.wantBehind)
		}
	}

	// Diverged: a second child of c1 on another branch — each side has one
	// commit the other lacks.
	pgit(t, repo, "checkout", "-qb", "other", c1)
	pWrite(t, filepath.Join(repo, "g.txt"), "branchB")
	pgit(t, repo, "add", ".")
	pgit(t, repo, "commit", "-qm", "c2b")
	c2b := pRev(t, repo, "HEAD")
	if got := classifyDrift(repo, c2, c2b); got.state != driftDiverged || got.ahead != 1 || got.behind != 1 {
		t.Errorf("diverged: classifyDrift = %+v, want {state:%d ahead:1 behind:1}", got, driftDiverged)
	}
}

// TestRenderStatusReport_DriftDirection pins that each drift direction
// renders the right wording + the right verb(s).
func TestRenderStatusReport_DriftDirection(t *testing.T) {
	t.Parallel()
	base := statusReport{
		WorktreeID:         "wt",
		WorktreeDir:        "repo",
		ActiveRemote:       "dev",
		ActiveRemoteURL:    "http://localhost:7878",
		SignedIn:           true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{ID: "wt"},
		HasCheckpoint:      true,
		InSync:             false,
	}
	cases := []struct {
		drift              driftState
		want               string
		wantPush, wantPull bool
	}{
		{driftAhead, "Ahead of dev remote", true, false},
		{driftBehind, "Behind dev remote", false, true},
		{driftDiverged, "Diverged from dev remote", true, true},
		{driftUnknown, "Out of sync with dev remote", true, true},
	}
	for _, tc := range cases {
		rep := base
		rep.Drift = tc.drift
		got := stripANSI(renderStatusReport(rep))
		if !strings.Contains(got, tc.want) {
			t.Errorf("drift %d: missing %q in:\n%s", tc.drift, tc.want, got)
		}
		if strings.Contains(got, "`clank push`") != tc.wantPush {
			t.Errorf("drift %d: push hint = %v, want %v", tc.drift, !tc.wantPush, tc.wantPush)
		}
		if strings.Contains(got, "`clank pull`") != tc.wantPull {
			t.Errorf("drift %d: pull hint = %v, want %v", tc.drift, !tc.wantPull, tc.wantPull)
		}
	}
}

// TestRenderStatusReport_DriftCounts pins the commit-count wording,
// including pluralization and the diverged two-number form.
func TestRenderStatusReport_DriftCounts(t *testing.T) {
	t.Parallel()
	base := statusReport{
		WorktreeID:         "wt",
		WorktreeDir:        "repo",
		ActiveRemote:       "dev",
		ActiveRemoteURL:    "http://localhost:7878",
		SignedIn:           true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{ID: "wt"},
		HasCheckpoint:      true,
		InSync:             false,
	}
	cases := []struct {
		name   string
		mutate func(*statusReport)
		want   string
	}{
		{"ahead plural", func(r *statusReport) { r.Drift = driftAhead; r.DriftAhead = 3 }, "Ahead of dev remote by 3 commits"},
		{"behind singular", func(r *statusReport) { r.Drift = driftBehind; r.DriftBehind = 1 }, "Behind dev remote by 1 commit"},
		{"ahead uncommitted (no count)", func(r *statusReport) { r.Drift = driftAhead }, "Ahead of dev remote — "},
		{"diverged counts", func(r *statusReport) { r.Drift = driftDiverged; r.DriftAhead = 2; r.DriftBehind = 1 }, "Diverged from dev remote (2 ahead, 1 behind)"},
	}
	for _, tc := range cases {
		rep := base
		tc.mutate(&rep)
		got := stripANSI(renderStatusReport(rep))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: missing %q in:\n%s", tc.name, tc.want, got)
		}
	}
}
