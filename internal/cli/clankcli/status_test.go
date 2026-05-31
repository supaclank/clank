package clankcli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/cloud"
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

func TestRenderStatusReport_InSync(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		WorktreeDir:     "mindmouth",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{
			ID: "quizzical-keller-80cdc4",
		},
		HasCheckpoint: true,
		InSync:        true,
	}))
	wantAll := []string{
		"On worktree mindmouth",
		"In sync with dev remote",
	}
	for _, s := range wantAll {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in output:\n%s", s, got)
		}
	}
}

func TestRenderStatusReport_OutOfSync(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		WorktreeDir:     "mindmouth",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{
			ID: "quizzical-keller-80cdc4",
		},
		HasCheckpoint: true,
		InSync:        false,
	}))
	if !strings.Contains(got, "Out of sync") {
		t.Errorf("expected 'Out of sync'; got:\n%s", got)
	}
	// Direction is undeterminable here (no Drift set), so both verbs surface.
	if !strings.Contains(got, "`clank push`") || !strings.Contains(got, "`clank pull`") {
		t.Errorf("expected both push and pull hints; got:\n%s", got)
	}
}

func TestRenderStatusReport_NoCheckpoint(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "happy-curie-7f0a11",
		WorktreeDir:     "mindmouth",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{
			ID: "happy-curie-7f0a11",
		},
		HasCheckpoint: false,
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
		SignedIn:        true, // already authenticated; just hasn't pushed yet
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
		WorktreeID:  "quizzical-keller-80cdc4",
		WorktreeDir: "mindmouth",
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
		WorktreeDir:     "mindmouth",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        true,
		RemoteError:     errors.New("dial tcp: connection refused"),
	}))
	if !strings.Contains(got, "unknown") || !strings.Contains(got, "unreachable") {
		t.Errorf("expected 'unknown' + 'unreachable'; got:\n%s", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("expected the wrapped error to surface; got:\n%s", got)
	}
}

// TestRenderStatusReport_WorktreeRemovedFromRemote: a local worktree id
// present but absent from the remote's list collapses to the same
// "Not synced — run clank push" output as never-pushed. The user
// doesn't care about the distinction; both fix the same way.
func TestRenderStatusReport_WorktreeRemovedFromRemote(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		WorktreeDir:     "mindmouth",
		ActiveRemote:    "dev",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        true,
		// WorktreeFromRemote intentionally nil — ListWorktrees succeeded
		// but didn't include this worktree.
	}))
	if !strings.Contains(got, "Not synced") {
		t.Errorf("expected 'Not synced'; got:\n%s", got)
	}
	if !strings.Contains(got, "to the dev remote") {
		t.Errorf("expected the push hint to target the dev remote; got:\n%s", got)
	}
	if strings.Contains(got, "On worktree") {
		t.Errorf("must not show 'On worktree' headline when push is needed; got:\n%s", got)
	}
}

// TestRenderStatusReport_NoWorktreeID_NotSignedIn pins the regression
// where a fresh repo with no cached worktree id used to tell the user
// "Run `clank push`" — but if they aren't signed in, push 401s on
// RegisterWorktree. The login check must beat the push hint.
func TestRenderStatusReport_NoWorktreeID_NotSignedIn(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "",
		ActiveRemote:    "supaclank-dev",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        false,
	}))
	if !strings.Contains(got, "Not signed in") {
		t.Errorf("expected 'Not signed in'; got:\n%s", got)
	}
	if !strings.Contains(got, "`clank login`") {
		t.Errorf("expected hint to run `clank login`; got:\n%s", got)
	}
	if strings.Contains(got, "clank push") {
		t.Errorf("must not suggest push when user isn't signed in; got:\n%s", got)
	}
}

// TestRenderStatusReport_NotSignedIn pins the regression for the
// pre-existing UX bug where an unauthenticated session printed
// "remote unreachable: ... 401: unauthorized" — confusing because the
// remote was perfectly reachable; the user just hadn't logged in.
func TestRenderStatusReport_NotSignedIn(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		WorktreeDir:     "mindmouth",
		ActiveRemote:    "supaclank-local",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        false,
	}))
	if !strings.Contains(got, "Not signed in") {
		t.Errorf("expected 'Not signed in'; got:\n%s", got)
	}
	if !strings.Contains(got, "supaclank-local") {
		t.Errorf("expected remote name 'supaclank-local'; got:\n%s", got)
	}
	if !strings.Contains(got, "`clank login`") {
		t.Errorf("expected hint to run `clank login`; got:\n%s", got)
	}
	if strings.Contains(got, "unreachable") {
		t.Errorf("must not say 'unreachable' when remote is reachable but auth failed; got:\n%s", got)
	}
}

// TestRenderStatusReport_Unauthorized covers the case where the token
// is present but the gateway rejected it (e.g. expired session that
// couldn't be refreshed). Same user-facing message as the no-token
// case — the fix is identical: run `clank login`.
func TestRenderStatusReport_Unauthorized(t *testing.T) {
	t.Parallel()
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:      "quizzical-keller-80cdc4",
		WorktreeDir:     "mindmouth",
		ActiveRemote:    "supaclank-local",
		ActiveRemoteURL: "http://localhost:7878",
		SignedIn:        true,
		RemoteError:     fmt.Errorf("list worktrees: %w", cloud.ErrUnauthorized),
	}))
	if !strings.Contains(got, "Not signed in") {
		t.Errorf("expected 'Not signed in'; got:\n%s", got)
	}
	if strings.Contains(got, "unreachable") {
		t.Errorf("401 should not be reported as 'unreachable'; got:\n%s", got)
	}
}

// TestRenderStatusReport_DirectoryHeadline is a regression for the
// "worktree ULID exposed to users" issue — the headline should show
// the directory basename, never the ULID.
func TestRenderStatusReport_DirectoryHeadline(t *testing.T) {
	t.Parallel()
	const ulid = "01KRX7M2JFBHQFKZVJDEDHGVAW"
	got := stripANSI(renderStatusReport(statusReport{
		WorktreeID:  ulid,
		WorktreeDir: "mindmouth",
		// No remote configured, so the simplest branch.
	}))
	if !strings.Contains(got, "On worktree mindmouth") {
		t.Errorf("expected 'On worktree mindmouth' headline; got:\n%s", got)
	}
	if strings.Contains(got, ulid) {
		t.Errorf("ULID must not appear in user-facing output; got:\n%s", got)
	}
}
