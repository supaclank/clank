package clankcli

import (
	"errors"
	"strings"
	"testing"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// stripANSI removes ANSI escape codes so test assertions can match on
// the textual content regardless of lipgloss's TTY-detection.
// lipgloss DOES strip when running in `go test` (no tty), but if it
// ever changes behaviour the assertions stay stable.
func stripANSI(s string) string {
	var sb strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

func TestRenderStatusReport_RemoteOwned(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		WorktreeFromRemote: &daemonclient.WorktreeInfo{
			ID:                     "quizzical-keller-80cdc4",
			OwnerKind:              "remote",
			LatestSyncedCheckpoint: "01ABCDEFGHJKMNPQRSTVWXYZ12",
		},
	}))
	wantAll := []string{
		"On worktree quizzical-keller-80cdc4",
		"Owned by dev remote",
		"localhost:7878",
		"Latest checkpoint: 01ABCDEFGHJKMNPQRSTVWXYZ12",
	}
	for _, s := range wantAll {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in output:\n%s", s, got)
		}
	}
}

func TestRenderStatusReport_LocalOwned_WithCheckpoint(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		WorktreeFromRemote: &daemonclient.WorktreeInfo{
			ID:                     "quizzical-keller-80cdc4",
			OwnerKind:              "local",
			LatestSyncedCheckpoint: "ck-abc",
		},
	}))
	if !strings.Contains(got, "Owned by this laptop") {
		t.Errorf("expected 'Owned by this laptop'; got:\n%s", got)
	}
	if !strings.Contains(got, "Synced to dev remote") {
		t.Errorf("expected 'Synced to dev remote' line; got:\n%s", got)
	}
	if strings.Contains(got, "Owned by dev remote") {
		t.Errorf("should not say remote-owned:\n%s", got)
	}
}

func TestRenderStatusReport_LocalOwned_NoCheckpoint(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "happy-curie-7f0a11",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		WorktreeFromRemote: &daemonclient.WorktreeInfo{
			ID:        "happy-curie-7f0a11",
			OwnerKind: "local",
		},
	}))
	if !strings.Contains(got, "Not yet pushed to dev remote") {
		t.Errorf("expected 'Not yet pushed to dev remote'; got:\n%s", got)
	}
}

func TestRenderStatusReport_NotRegistered_NoRemote(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID: "",
	}))
	if !strings.Contains(got, "Not synced") {
		t.Errorf("expected 'Not synced'; got:\n%s", got)
	}
	if !strings.Contains(got, "clank remote add") {
		t.Errorf("expected hint to add a remote; got:\n%s", got)
	}
	if strings.Contains(got, "Owner") {
		t.Errorf("no ownership row should appear when not registered:\n%s", got)
	}
}

func TestRenderStatusReport_NotRegistered_WithRemote(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
	}))
	if !strings.Contains(got, "Run `clank push`") {
		t.Errorf("expected push hint; got:\n%s", got)
	}
	if !strings.Contains(got, "to the dev remote") || strings.Contains(got, "clank remote add") {
		t.Errorf("hint should target the configured remote, not suggest re-adding one; got:\n%s", got)
	}
}

func TestRenderStatusReport_LocalOnly_NoRemote(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID: "quizzical-keller-80cdc4",
		// ActiveRemote intentionally empty
	}))
	if !strings.Contains(got, "Owned by this laptop") {
		t.Errorf("expected local ownership; got:\n%s", got)
	}
	if !strings.Contains(got, "no remote configured") {
		t.Errorf("expected '(no remote configured)' suffix; got:\n%s", got)
	}
}

func TestRenderStatusReport_RemoteUnreachable(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		RemoteError:     errors.New("dial tcp: connection refused"),
	}))
	if !strings.Contains(got, "unknown") || !strings.Contains(got, "unreachable") {
		t.Errorf("expected 'unknown' + 'unreachable'; got:\n%s", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("expected the wrapped error to surface; got:\n%s", got)
	}
}

func TestRenderStatusReport_WorktreeRemovedFromRemote(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		// WorktreeFromRemote intentionally nil — ListWorktrees succeeded
		// but didn't include this worktree.
	}))
	if !strings.Contains(got, "Not on") || !strings.Contains(got, "dev remote") {
		t.Errorf("expected 'Not on … dev remote'; got:\n%s", got)
	}
	if !strings.Contains(got, "clank push") {
		t.Errorf("expected 're-register' hint; got:\n%s", got)
	}
}
