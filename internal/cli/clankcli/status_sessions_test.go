package clankcli

import (
	"strings"
	"testing"
	"time"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/pkg/sessionsync"
)

// TestRenderStatusReport_LastPushed pins the recency hint: the "last pushed
// <ago>" headline suffix appears once a push has been recorded, and is
// omitted when this machine has never pushed.
func TestRenderStatusReport_LastPushed(t *testing.T) {
	t.Parallel()
	base := statusReport{
		WorktreeID: "wt", WorktreeDir: "repo", ActiveRemote: "dev", SignedIn: true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{ID: "wt"}, HasCheckpoint: true, InSync: true,
	}

	t.Run("shows last pushed when recorded", func(t *testing.T) {
		t.Parallel()
		r := base
		r.LastPushedAt = time.Now().Add(-5 * time.Minute)
		got := stripANSI(renderStatusReport(r))
		if !strings.Contains(got, "last pushed") {
			t.Errorf("want last-pushed headline suffix:\n%s", got)
		}
	})

	t.Run("omitted when never pushed", func(t *testing.T) {
		t.Parallel()
		got := stripANSI(renderStatusReport(base)) // LastPushedAt zero
		if strings.Contains(got, "last pushed") {
			t.Errorf("never-pushed status leaked a last-pushed line:\n%s", got)
		}
	})
}

func pushCTACount(s string) int { return strings.Count(s, "`clank push`") }

// TestRenderStatusReport_Sessions pins the sessions axis: hidden when
// unknown, "✓ Sessions in sync" when current, "• N session(s) not synced"
// otherwise, and a push CTA exactly once even when code is otherwise synced.
func TestRenderStatusReport_Sessions(t *testing.T) {
	t.Parallel()
	base := statusReport{
		WorktreeID: "wt", WorktreeDir: "repo", ActiveRemote: "dev", SignedIn: true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{ID: "wt"}, HasCheckpoint: true, InSync: true,
	}

	t.Run("unknown axis omits the sessions line", func(t *testing.T) {
		t.Parallel()
		got := stripANSI(renderStatusReport(base)) // SessionsKnown false
		if strings.Contains(strings.ToLower(got), "session") {
			t.Errorf("no sessions line expected:\n%s", got)
		}
		if !strings.Contains(got, "In sync with dev remote") {
			t.Errorf("want compact in-sync summary:\n%s", got)
		}
	})

	t.Run("in sync shows ✓ Sessions in sync, no CTA", func(t *testing.T) {
		t.Parallel()
		r := base
		r.SessionsKnown = true // 0 unsynced
		got := stripANSI(renderStatusReport(r))
		for _, want := range []string{"✓ Commits in sync", "✓ Sessions in sync"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
		if pushCTACount(got) != 0 {
			t.Errorf("no CTA expected when everything synced:\n%s", got)
		}
	})

	t.Run("sessions lag while code in sync → exactly one push CTA", func(t *testing.T) {
		t.Parallel()
		r := base
		r.SessionsKnown = true
		r.UnsyncedSessions = 1
		got := stripANSI(renderStatusReport(r))
		if !strings.Contains(got, "✓ Commits in sync") || !strings.Contains(got, "1 session not synced") {
			t.Errorf("want commits-synced + singular session line:\n%s", got)
		}
		if n := pushCTACount(got); n != 1 {
			t.Errorf("want exactly one push CTA, got %d:\n%s", n, got)
		}
	})

	t.Run("plural", func(t *testing.T) {
		t.Parallel()
		r := base
		r.SessionsKnown = true
		r.UnsyncedSessions = 3
		if got := stripANSI(renderStatusReport(r)); !strings.Contains(got, "3 sessions not synced") {
			t.Errorf("want plural:\n%s", got)
		}
	})

	t.Run("rides alongside commit drift with a single CTA", func(t *testing.T) {
		t.Parallel()
		r := base
		r.InSync = false
		r.Drift = driftAhead
		r.DriftAhead = 1
		r.SessionsKnown = true
		r.UnsyncedSessions = 2
		got := stripANSI(renderStatusReport(r))
		for _, want := range []string{"Ahead by 1 commit", "2 sessions not synced"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
		if n := pushCTACount(got); n != 1 {
			t.Errorf("want exactly one push CTA, got %d:\n%s", n, got)
		}
	})
}

// TestSessionLabels pins the -v detail labels: quoted title, or "session
// <id>" when untitled.
func TestSessionLabels(t *testing.T) {
	t.Parallel()
	got := sessionLabels([]sessionsync.DiscoveredSession{
		{ExternalID: "ses_1", Title: "Refactor the auth flow"},
		{ExternalID: "ses_2"},
	})
	want := []string{`"Refactor the auth flow"`, "session ses_2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sessionLabels = %v, want %v", got, want)
	}
}

// TestRenderStatusReport_Verbose pins that -v lists the changed session
// titles under the sessions bullet, and that without -v they're hidden.
func TestRenderStatusReport_Verbose(t *testing.T) {
	t.Parallel()
	rep := statusReport{
		WorktreeID: "wt", WorktreeDir: "repo", ActiveRemote: "dev", SignedIn: true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{ID: "wt"}, HasCheckpoint: true, InSync: true,
		SessionsKnown: true, UnsyncedSessions: 2,
		Verbose:               true,
		UnsyncedSessionLabels: []string{`"Refactor the auth flow"`, `"Debug the flaky test"`},
	}
	got := stripANSI(renderStatusReport(rep))
	for _, want := range []string{`"Refactor the auth flow"`, `"Debug the flaky test"`} {
		if !strings.Contains(got, want) {
			t.Errorf("verbose session detail missing %q:\n%s", want, got)
		}
	}

	// Same report without -v hides the titles (just the count bullet).
	rep.Verbose = false
	got = stripANSI(renderStatusReport(rep))
	if strings.Contains(got, "Refactor the auth flow") {
		t.Errorf("non-verbose output leaked session titles:\n%s", got)
	}
	if !strings.Contains(got, "2 sessions not synced") {
		t.Errorf("count bullet should still show:\n%s", got)
	}
}
