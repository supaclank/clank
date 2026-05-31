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
	}{
		{"local ahead of remote", c2, c1, driftAhead},
		{"local behind remote", c1, c2, driftBehind},
		{"same head (uncommitted drift)", c2, c2, driftAhead},
		{"remote head unknown locally", c2, "1234567890123456789012345678901234567890", driftBehind},
	}
	for _, tc := range cases {
		if got := classifyDrift(repo, tc.local, tc.remote); got != tc.want {
			t.Errorf("%s: classifyDrift = %d, want %d", tc.name, got, tc.want)
		}
	}

	// Diverged: a second child of c1 on another branch — neither head is
	// an ancestor of the other.
	pgit(t, repo, "checkout", "-qb", "other", c1)
	pWrite(t, filepath.Join(repo, "g.txt"), "branchB")
	pgit(t, repo, "add", ".")
	pgit(t, repo, "commit", "-qm", "c2b")
	c2b := pRev(t, repo, "HEAD")
	if got := classifyDrift(repo, c2, c2b); got != driftDiverged {
		t.Errorf("diverged: classifyDrift = %d, want %d", got, driftDiverged)
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
