package host_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// TestRegisterImportedSession_RoundTrip exercises the full
// import → upsert flow with a real opencode binary in an isolated
// HOME. Verifies:
//   - opencode import preserves external_id from the seed blob
//   - SessionInfo row is upserted with the manifest's metadata
//   - re-running is idempotent (status idle, no duplicate row)
func TestRegisterImportedSession_RoundTrip(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local/share/opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config/opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	const externalID = "ses_registerimportroundtripxxxx"
	const sessULID = "01HREGISTERIMPORTROUNDTRIP000"
	const worktreeID = "wt-register-import"

	blob := buildSyntheticOCBlob(externalID, "msg_registerimportseed00000000", "build", "hello import")
	blobPath := filepath.Join(t.TempDir(), "blob.json")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	now := time.Now()
	entry := checkpoint.SessionEntry{
		SessionID:      sessULID,
		ExternalID:     externalID,
		Backend:        agent.BackendOpenCode,
		BlobKey:        "sessions/" + sessULID + ".json",
		Status:         agent.StatusIdle,
		Title:          "register import roundtrip",
		Prompt:         "the original prompt",
		TicketID:       "JIRA-1",
		Agent:          "build",
		WorktreeBranch: "feature/x",
		CreatedAt:      now.Add(-time.Hour),
		UpdatedAt:      now.Add(-time.Minute),
		Bytes:          int64(len(blob)),
		WasBusy:        false,
	}

	got, err := svc.RegisterImportedSession(context.Background(), worktreeID, entry, blobPath)
	if err != nil {
		t.Fatalf("RegisterImportedSession: %v", err)
	}
	if got.ID != sessULID {
		t.Errorf("got.ID = %q, want %q", got.ID, sessULID)
	}
	if got.ExternalID != externalID {
		t.Errorf("got.ExternalID = %q, want %q (opencode must preserve info.id)", got.ExternalID, externalID)
	}
	if got.Backend != agent.BackendOpenCode {
		t.Errorf("got.Backend = %q", got.Backend)
	}
	if got.Status != agent.StatusIdle {
		t.Errorf("got.Status = %q, want idle (imported sessions always idle)", got.Status)
	}
	if got.GitRef.WorktreeID != worktreeID {
		t.Errorf("got.GitRef.WorktreeID = %q, want %q", got.GitRef.WorktreeID, worktreeID)
	}
	if got.GitRef.WorktreeBranch != entry.WorktreeBranch {
		t.Errorf("got.GitRef.WorktreeBranch = %q", got.GitRef.WorktreeBranch)
	}
	if got.Title != entry.Title {
		t.Errorf("got.Title = %q, want %q", got.Title, entry.Title)
	}
	if got.TicketID != entry.TicketID {
		t.Errorf("got.TicketID = %q", got.TicketID)
	}

	persisted, err := st.GetSession(context.Background(), sessULID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if persisted.ID != got.ID || persisted.ExternalID != got.ExternalID {
		t.Errorf("persisted row mismatch:\n got %+v\n want %+v", persisted, got)
	}

	// Idempotent re-run: a second call should not error and the row
	// should still match (no duplicate row).
	got2, err := svc.RegisterImportedSession(context.Background(), worktreeID, entry, blobPath)
	if err != nil {
		t.Fatalf("RegisterImportedSession (re-run): %v", err)
	}
	if got2.ID != sessULID || got2.ExternalID != externalID {
		t.Errorf("re-run mismatch: ID=%q ExternalID=%q", got2.ID, got2.ExternalID)
	}

	all, err := st.ListSessionsByWorktree(context.Background(), worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("after idempotent re-run, expected 1 session in worktree, got %d", len(all))
	}
}

// TestRegisterImportedSession_RewritesDirectoryToDestination is a
// regression test for the opencode "global project" cosmetic issue:
// imported sessions used to land under projectId:"global" because
// info.directory in the blob held the SOURCE's local path. After
// the rewrite shim, info.directory must reflect the DESTINATION's
// worktree path, and projectID must NOT match the source's hash
// (proxy for "opencode rederived from the new directory").
//
// Workaround pinned by https://github.com/anomalyco/opencode/issues/15797
// and https://github.com/anomalyco/opencode/pull/15826. Once that
// lands upstream, both the production rewrite and this test can be
// removed.
func TestRegisterImportedSession_RewritesDirectoryToDestination(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local/share/opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config/opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	// Override the workRootDir so we can assert against a deterministic
	// destination path without depending on $HOME/work existing.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	const externalID = "ses_dirrewrite00000000000000000"
	const sessULID = "01HDIRREWRITE00000000000000000"
	const worktreeID = "wt-dirrewrite"
	const sourceDir = "/Users/somebody/elsewhere/repo"

	// Build the seed blob with the SOURCE's directory baked in, plus a
	// known projectID we can detect getting rewritten.
	blob := buildOCBlobWithDir(externalID, "msg_dirrewriteseed0000000000000", "build", "hello", sourceDir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	blobPath := filepath.Join(t.TempDir(), "blob.json")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	entry := checkpoint.SessionEntry{
		SessionID:  sessULID,
		ExternalID: externalID,
		Backend:    agent.BackendOpenCode,
		ProjectDir: sourceDir,
		Status:     agent.StatusIdle,
	}
	if _, err := svc.RegisterImportedSession(context.Background(), worktreeID, entry, blobPath); err != nil {
		t.Fatalf("RegisterImportedSession: %v", err)
	}

	// Re-export the imported session and inspect info.directory.
	exportCmd := exec.Command("opencode", "export", externalID)
	out, err := exportCmd.Output()
	if err != nil {
		t.Fatalf("opencode export: %v", err)
	}
	var got struct {
		Info struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
			ProjectID string `json:"projectID"`
		} `json:"info"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse re-export: %v", err)
	}
	wantDir := filepath.Join(workRoot, worktreeID)
	if got.Info.Directory != wantDir {
		t.Errorf("info.directory = %q, want %q (rewrite shim failed)", got.Info.Directory, wantDir)
	}
	if got.Info.ProjectID == "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("info.projectID preserved the source's hash; opencode did not rederive")
	}
}

// TestRegisterImportedSession_IgnoresSourceProjectDir is a regression
// test for the chdir-into-nonexistent-source-path bug: on push --migrate,
// the laptop stamps SessionEntry.ProjectDir with its own local path,
// which is meaningless on the destination (sprite). The destination
// MUST NOT pass that path as cmd.Dir to opencode, or exec.Command
// fails with `chdir … no such file or directory` before opencode
// even runs — observed in production on the dev stack.
func TestRegisterImportedSession_IgnoresSourceProjectDir(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local/share/opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config/opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	const externalID = "ses_chdirregression000000000000"
	const sessULID = "01HCHDIRREGRESSION0000000000"
	const worktreeID = "wt-chdir"

	blob := buildSyntheticOCBlob(externalID, "msg_chdirseed00000000000000000", "build", "hello")
	blobPath := filepath.Join(t.TempDir(), "blob.json")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	// SessionEntry.ProjectDir is the SOURCE's local path — a path that
	// does NOT exist on this host. RegisterImportedSession must succeed
	// regardless.
	entry := checkpoint.SessionEntry{
		SessionID:  sessULID,
		ExternalID: externalID,
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/path/that/does/not/exist/on/destination",
		Status:     agent.StatusIdle,
		Title:      "chdir regression",
	}

	got, err := svc.RegisterImportedSession(context.Background(), worktreeID, entry, blobPath)
	if err != nil {
		t.Fatalf("RegisterImportedSession with nonexistent ProjectDir: %v", err)
	}
	if got.ExternalID != externalID {
		t.Errorf("ExternalID=%q want %q", got.ExternalID, externalID)
	}
}

// TestRegisterImportedSession_RejectsClaudeBackend pins v1 scope.
func TestRegisterImportedSession_RejectsClaudeBackend(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	_, err = svc.RegisterImportedSession(context.Background(), "wt", checkpoint.SessionEntry{
		SessionID: "01H",
		Backend:   agent.BackendClaudeCode,
	}, "/nonexistent")
	if err == nil {
		t.Fatal("expected error for claude-code backend")
	}
}

func TestRegisterImportedSession_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	entry := checkpoint.SessionEntry{
		SessionID: "01H",
		Backend:   agent.BackendOpenCode,
	}
	if _, err := svc.RegisterImportedSession(context.Background(), "", entry, "/tmp/x"); err == nil {
		t.Errorf("expected error for empty worktreeID")
	}
	if _, err := svc.RegisterImportedSession(context.Background(), "wt", checkpoint.SessionEntry{Backend: agent.BackendOpenCode}, "/tmp/x"); err == nil {
		t.Errorf("expected error for empty SessionID")
	}
}
