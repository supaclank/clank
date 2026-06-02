package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// TestExportSessions_ExportsClaude is the flip of the old "skips claude"
// behavior: a claude-code session in host.db is now exported, not skipped.
// We seed it via RegisterImportedSession (which writes the transcript under
// the worktree's encoded path) and assert ExportSessions reads it back —
// the sprite leg of the round trip. No claude binary needed; isolated
// CLAUDE_CONFIG_DIR + workRoot (not parallel — t.Setenv + workRoot singleton).
func TestExportSessions_ExportsClaude(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	const externalID = "claude-host-export-0000-1111"
	const sessULID = "01HCLAUDEHOSTEXPORT0000000000"
	const worktreeID = "wt-claude-export"

	// Seed the on-disk transcript + host.db row by importing (the same path
	// a laptop→sprite push apply takes). cwd inside the blob is a source
	// path; import rebases it under workRoot/worktreeID.
	blob := buildSyntheticClaudeBlob(externalID, "/laptop/source/path")
	blobPath := filepath.Join(t.TempDir(), "seed.jsonl")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterImportedSession(context.Background(), worktreeID, checkpoint.SessionEntry{
		SessionID:  sessULID,
		ExternalID: externalID,
		Backend:    agent.BackendClaudeCode,
		Status:     agent.StatusIdle,
		Title:      "claude export",
	}, blobPath); err != nil {
		t.Fatalf("seed RegisterImportedSession: %v", err)
	}

	res, err := svc.ExportSessions(context.Background(), worktreeID, "ck-claude")
	if err != nil {
		t.Fatalf("ExportSessions: %v", err)
	}
	t.Cleanup(res.Cleanup)

	if len(res.Skipped) != 0 {
		t.Errorf("expected 0 skipped (claude now exported), got %d: %+v", len(res.Skipped), res.Skipped)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("expected 1 manifest entry, got %d", len(res.Entries))
	}
	e := res.Entries[0]
	if e.SessionID != sessULID {
		t.Errorf("entry.SessionID = %q, want %q", e.SessionID, sessULID)
	}
	if e.ExternalID != externalID {
		t.Errorf("entry.ExternalID = %q, want %q (Claude preserves the session id)", e.ExternalID, externalID)
	}
	if e.Backend != agent.BackendClaudeCode {
		t.Errorf("entry.Backend = %q, want claude-code", e.Backend)
	}
	if e.Bytes <= 0 {
		t.Errorf("entry.Bytes = %d, want positive", e.Bytes)
	}
}

// TestExportSessions_EmptyWorktree: no sessions in the worktree
// returns an empty result, not an error.
func TestExportSessions_EmptyWorktree(t *testing.T) {
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

	res, err := svc.ExportSessions(context.Background(), "wt-nothing", "ck-empty")
	if err != nil {
		t.Fatalf("ExportSessions: %v", err)
	}
	t.Cleanup(res.Cleanup)

	if len(res.Entries) != 0 {
		t.Errorf("want 0 entries, got %d", len(res.Entries))
	}
	if len(res.Skipped) != 0 {
		t.Errorf("want 0 skipped, got %d", len(res.Skipped))
	}
}

// TestExportSessions_RejectsEmptyArgs: input-validation guards.
func TestExportSessions_RejectsEmptyArgs(t *testing.T) {
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

	if _, err := svc.ExportSessions(context.Background(), "", "ck"); err == nil {
		t.Errorf("expected error for empty worktreeID")
	}
	if _, err := svc.ExportSessions(context.Background(), "wt", ""); err == nil {
		t.Errorf("expected error for empty checkpointID")
	}
}

// TestExportSessions_OpenCodeHappyPath integration-tests the full
// quiesce-and-export flow against a real opencode binary. Skips if
// opencode is not on $PATH.
//
// Per CLAUDE.md "NEVER mock dependencies", this exercises the real
// CLI in an isolated HOME with a synthetic seed blob — no LLM round
// trip required.
func TestExportSessions_OpenCodeHappyPath(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	// Isolated opencode HOME so we don't touch the user's storage.
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

	// Seed an opencode session via synthetic blob import.
	const externalID = "ses_exporttesthappypath00000000"
	seed := buildSyntheticOCBlob(externalID, "msg_exporttestseed000000000000", "build", "hello")
	seedPath := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seedPath, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	importedID, err := agent.OpenCodeImportSession(context.Background(), "", seedPath)
	if err != nil {
		t.Fatalf("seed import: %v", err)
	}
	if importedID != externalID {
		t.Fatalf("seed import returned %q, want %q", importedID, externalID)
	}

	// Register the session in host.db so ListSessionsByWorktree picks it up.
	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const sessULID = "01HEXPORTHAPPY0000000000000000"
	const worktreeID = "wt-export-happy"
	now := time.Now()
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID:         sessULID,
		ExternalID: externalID,
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{WorktreeID: worktreeID},
		Title:      "export-happy",
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	res, err := svc.ExportSessions(context.Background(), worktreeID, "ck-happy")
	if err != nil {
		t.Fatalf("ExportSessions: %v", err)
	}
	t.Cleanup(res.Cleanup)

	if len(res.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(res.Entries))
	}
	if len(res.Skipped) != 0 {
		t.Errorf("want 0 skipped, got %d", len(res.Skipped))
	}
	e := res.Entries[0]
	if e.SessionID != sessULID {
		t.Errorf("entry.SessionID = %q, want %q", e.SessionID, sessULID)
	}
	if e.ExternalID != externalID {
		t.Errorf("entry.ExternalID = %q, want %q", e.ExternalID, externalID)
	}
	if e.Backend != agent.BackendOpenCode {
		t.Errorf("entry.Backend = %q, want opencode", e.Backend)
	}
	if e.BlobKey != "sessions/"+sessULID+".json" {
		t.Errorf("entry.BlobKey = %q, want sessions/%s.json", e.BlobKey, sessULID)
	}
	if e.Bytes <= 0 {
		t.Errorf("entry.Bytes = %d, want positive", e.Bytes)
	}
	if e.WasBusy {
		t.Errorf("entry.WasBusy = true for an idle session")
	}

	blobPath, ok := res.BlobPaths[sessULID]
	if !ok {
		t.Fatalf("BlobPaths missing %s", sessULID)
	}
	blobData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	var parsed struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(blobData, &parsed); err != nil {
		t.Fatalf("parse blob: %v", err)
	}
	if parsed.Info.ID != externalID {
		t.Errorf("blob info.id = %q, want %q", parsed.Info.ID, externalID)
	}
}

// TestExportSessions_SkipsMissingOpencodeSession is the regression
// test for the host.db-orphan case: a SessionInfo row exists in
// host.db but `opencode export <external_id>` says "Session not
// found" because someone deleted the session via opencode CLI
// directly. ExportSessions used to abort the entire migration on
// the first such row; now it must skip the orphan with a warning
// and keep going.
//
// We seed two sessions in host.db: one whose external_id refers
// to a real opencode session, one whose external_id is a made-up
// ID opencode has never seen. Then we assert ExportSessions
// returns one entry (the real one) and one Skipped entry (the
// orphan), with no error.
func TestExportSessions_SkipsMissingOpencodeSession(t *testing.T) {
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

	// Seed ONE real opencode session by importing a synthetic blob.
	const realExtID = "ses_orphanmix000000000000realll"
	seedBlob := buildSyntheticOCBlob(realExtID, "msg_orphanmix0000000000000realA", "build", "hello")
	seedPath := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seedPath, seedBlob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.OpenCodeImportSession(context.Background(), "", seedPath); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const worktreeID = "wt-orphan-mix"
	now := time.Now()

	// host.db row whose external_id maps to the real opencode session.
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID:         "01HORPHANMIXREAL00000000000",
		ExternalID: realExtID,
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{WorktreeID: worktreeID},
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("upsert real: %v", err)
	}

	// host.db row whose external_id refers to a session opencode has
	// never seen — the "orphan" case. This mimics what happens when
	// someone runs `opencode session delete` outside clank's view.
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID:         "01HORPHANMIXSTALE0000000000",
		ExternalID: "ses_orphanmix0000000000staleeee",
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{WorktreeID: worktreeID},
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("upsert orphan: %v", err)
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	res, err := svc.ExportSessions(context.Background(), worktreeID, "ck-orphan")
	if err != nil {
		t.Fatalf("ExportSessions must NOT return an error when one row is orphaned: %v", err)
	}
	t.Cleanup(res.Cleanup)

	if len(res.Entries) != 1 {
		t.Fatalf("want 1 entry (the real session), got %d", len(res.Entries))
	}
	if res.Entries[0].ExternalID != realExtID {
		t.Errorf("entry.ExternalID = %q, want the real one (%q)", res.Entries[0].ExternalID, realExtID)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("want 1 skipped (the orphan), got %d", len(res.Skipped))
	}
	if res.Skipped[0].SessionID != "01HORPHANMIXSTALE0000000000" {
		t.Errorf("Skipped.SessionID = %q, want orphan", res.Skipped[0].SessionID)
	}
	if !strings.Contains(res.Skipped[0].Reason, "orphan") && !strings.Contains(res.Skipped[0].Reason, "missing") {
		t.Errorf("Skipped.Reason should mention orphan/missing; got: %q", res.Skipped[0].Reason)
	}
}

// TestExportSessions_IgnoresStaleLocalPath is the regression test
// for the chdir-on-export bug observed during a pull-back: a
// session row whose host.db.project_dir is the SOURCE host's local
// path (because the row was imported earlier from that source) must
// still export cleanly on the DESTINATION. Without the fix, the
// sprite's Service.ExportSessions would chdir into the laptop's
// /Users/... path and fail before opencode could run.
//
// Mirrors TestRegisterImportedSession_IgnoresSourceProjectDir on
// the import side. Both fixes together close the same chdir-on-
// cross-machine-path footgun in both directions.
func TestExportSessions_IgnoresStaleLocalPath(t *testing.T) {
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

	// Seed an opencode session.
	const externalID = "ses_staleLocalPathRegress0000"
	blob := buildSyntheticOCBlob(externalID, "msg_staleseed00000000000000000", "build", "hello")
	seedPath := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seedPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.OpenCodeImportSession(context.Background(), "", seedPath); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	// Stamp the host.db row with a SOURCE-style LocalPath that
	// definitely doesn't exist on this destination.
	const sessULID = "01HSTALELOCALPATHREGRESS0000"
	const worktreeID = "wt-stale-localpath"
	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID:         sessULID,
		ExternalID: externalID,
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusIdle,
		GitRef: agent.GitRef{
			WorktreeID: worktreeID,
			LocalPath:  "/path/that/exists/only/on/another/machine",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	res, err := svc.ExportSessions(context.Background(), worktreeID, "ck-stale")
	if err != nil {
		t.Fatalf("ExportSessions with stale LocalPath: %v", err)
	}
	t.Cleanup(res.Cleanup)
	if len(res.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(res.Entries))
	}
	if res.Entries[0].SessionID != sessULID {
		t.Errorf("entry.SessionID = %q, want %q", res.Entries[0].SessionID, sessULID)
	}
}

// buildSyntheticClaudeBlob returns a minimal valid Claude JSONL transcript
// (a metadata line plus one message line carrying cwd) for sessionID.
// Mirrors the on-disk shape the Agent SDK reads.
func buildSyntheticClaudeBlob(sessionID, cwd string) []byte {
	lines := []map[string]any{
		{"type": "ai-title", "aiTitle": "host test", "sessionId": sessionID},
		{"type": "user", "sessionId": sessionID, "cwd": cwd, "gitBranch": "main", "uuid": "u-1", "message": map[string]any{"role": "user", "content": "hello"}},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			panic(err)
		}
	}
	return buf.Bytes()
}

// buildSyntheticOCBlob returns a minimal valid opencode session
// export JSON with one user message. Mirrors the schema seen on
// real exports of opencode 1.3.15.
func buildSyntheticOCBlob(sessID, msgID, agentSlug, text string) []byte {
	return buildOCBlobWithDir(sessID, msgID, agentSlug, text, "/tmp/clank-diag-test", "0000000000000000000000000000000000000000")
}

// buildOCBlobWithDir is the parameterized form of buildSyntheticOCBlob
// used by tests that need to assert specific directory / projectID
// values in the export (e.g. the directory-rewrite shim regression).
func buildOCBlobWithDir(sessID, msgID, agentSlug, text, directory, projectID string) []byte {
	body := map[string]any{
		"info": map[string]any{
			"id":        sessID,
			"slug":      "diag-slug",
			"projectID": projectID,
			"directory": directory,
			"title":     "diag",
			"version":   "1.3.15",
			"summary":   map[string]any{"additions": 0, "deletions": 0, "files": 0},
			"time":      map[string]any{"created": 1000, "updated": 1000},
		},
		"messages": []map[string]any{
			{
				"info": map[string]any{
					"id":        msgID,
					"sessionID": sessID,
					"role":      "user",
					"agent":     agentSlug,
					"model":     map[string]any{"providerID": "diag", "modelID": "diag"},
					"summary":   map[string]any{"diffs": []any{}},
					"time":      map[string]any{"created": 1000},
				},
				"parts": []map[string]any{
					{
						"type":      "text",
						"text":      text,
						"id":        "prt_" + msgID,
						"sessionID": sessID,
						"messageID": msgID,
					},
				},
			},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return b
}
