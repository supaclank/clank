package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func initGitRepoT(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@e.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	return dir
}

func TestSyncedSessions_RoundTrip(t *testing.T) {
	t.Parallel()
	repo := initGitRepoT(t)
	want := SyncedSession{Backend: BackendOpenCode, UpdatedAt: time.UnixMilli(1700000000000).UTC()}
	if err := WriteSyncedSessions(repo, SyncedSessionRecord{
		Sessions: map[string]SyncedSession{"ses_abc": want},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	gd, _ := GitDir(repo)
	if _, err := os.Stat(filepath.Join(gd, "clank", "sessions-synced.json")); err != nil {
		t.Fatalf("sidecar not at <gitDir>/clank/sessions-synced.json: %v", err)
	}

	got, err := ReadSyncedSessions(repo)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Version != SyncedSessionsVersion || len(got.Sessions) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	s := got.Sessions["ses_abc"]
	if s.Backend != BackendOpenCode || !s.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("session not round-tripped: %+v", got.Sessions)
	}
}

// TestReadSyncedSessions_BestEffort pins that an absent / corrupt / stale
// record degrades to an empty record with no error — status must never
// break on it.
func TestReadSyncedSessions_BestEffort(t *testing.T) {
	t.Parallel()

	if rec, err := ReadSyncedSessions(t.TempDir()); err != nil || len(rec.Sessions) != 0 {
		t.Errorf("non-git dir: rec=%+v err=%v, want empty/no-error", rec, err)
	}

	repo := initGitRepoT(t)
	if rec, err := ReadSyncedSessions(repo); err != nil || len(rec.Sessions) != 0 {
		t.Errorf("missing file: rec=%+v err=%v", rec, err)
	}

	gd, _ := GitDir(repo)
	dir := filepath.Join(gd, "clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions-synced.json")

	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec, err := ReadSyncedSessions(repo); err != nil || len(rec.Sessions) != 0 {
		t.Errorf("corrupt file: rec=%+v err=%v", rec, err)
	}

	if err := os.WriteFile(path, []byte(`{"version":999,"sessions":{"x":{"backend":"opencode"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec, err := ReadSyncedSessions(repo); err != nil || len(rec.Sessions) != 0 {
		t.Errorf("stale version: rec=%+v err=%v", rec, err)
	}
}
