package sessionsync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/agent"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

func TestClaudeDiscovered_Mapping(t *testing.T) {
	t.Parallel()
	cwd := "/Users/me/repo"
	created := int64(1_700_000_000_000)
	info := claudecode.SDKSessionInfo{
		SessionID:    "sess-123",
		Summary:      "Refactor auth",
		Cwd:          &cwd,
		LastModified: 1_700_000_100_000,
		CreatedAt:    &created,
	}
	got := claudeDiscovered(info)

	if got.Backend != agent.BackendClaudeCode {
		t.Errorf("Backend = %q, want %q", got.Backend, agent.BackendClaudeCode)
	}
	if got.ExternalID != "sess-123" {
		t.Errorf("ExternalID = %q, want sess-123", got.ExternalID)
	}
	if got.Title != "Refactor auth" {
		t.Errorf("Title = %q, want 'Refactor auth'", got.Title)
	}
	if got.ProjectDir != cwd {
		t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, cwd)
	}
	if got.CreatedAt.UnixMilli() != created {
		t.Errorf("CreatedAt = %d, want %d", got.CreatedAt.UnixMilli(), created)
	}
	if got.UpdatedAt.UnixMilli() != 1_700_000_100_000 {
		t.Errorf("UpdatedAt = %d, want 1700000100000", got.UpdatedAt.UnixMilli())
	}
}

func TestClaudeDiscovered_NilCwdAndCreatedAt(t *testing.T) {
	t.Parallel()
	info := claudecode.SDKSessionInfo{SessionID: "s", LastModified: 1234}
	got := claudeDiscovered(info)

	if got.ProjectDir != "" {
		t.Errorf("ProjectDir = %q, want empty when Cwd nil", got.ProjectDir)
	}
	if !got.CreatedAt.Equal(got.UpdatedAt) {
		t.Errorf("CreatedAt %v != UpdatedAt %v; want CreatedAt to fall back to UpdatedAt", got.CreatedAt, got.UpdatedAt)
	}
}

// TestClaudeBackend_RoundTrip exercises export → rewrite → import into a
// fresh worktree dir: the transcript is copied verbatim on export, rebased
// to the destination on import, lands at the destination's encoded path
// (discoverable by the SDK), keeps its session id, and re-imports
// idempotently. No claude binary needed — pure file I/O + the read-only SDK.
//
// Not parallel: isolates CLAUDE_CONFIG_DIR via t.Setenv.
func TestClaudeBackend_RoundTrip(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	ctx := context.Background()
	var be ClaudeBackend

	srcDir := t.TempDir()
	const sessionID = "roundtrip-1234-5678"
	writeClaudeTranscript(t, srcDir, sessionID)

	// Export is a verbatim copy of the on-disk transcript.
	var buf bytes.Buffer
	if err := be.ExportSession(ctx, srcDir, sessionID, &buf); err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	want, err := os.ReadFile(mustTranscriptPath(t, srcDir, sessionID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("ExportSession is not a verbatim copy of the transcript")
	}

	// Rebase for the destination, then import.
	destDir := t.TempDir()
	blobPath := filepath.Join(t.TempDir(), "blob.jsonl")
	if err := os.WriteFile(blobPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	rewritten, err := RewriteClaudeImportBlob(blobPath, destDir)
	if err != nil {
		t.Fatalf("RewriteClaudeImportBlob: %v", err)
	}
	gotID, err := be.ImportSession(ctx, destDir, rewritten)
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if gotID != sessionID {
		t.Errorf("imported id = %q, want %q (Claude preserves session identity)", gotID, sessionID)
	}

	// The transcript lands under destDir's encoded path; the SDK finds it.
	infos, err := claudecode.ListSessions(claudecode.WithSessionDirectory(destDir))
	if err != nil {
		t.Fatalf("ListSessions(destDir): %v", err)
	}
	if !containsSessionID(infos, sessionID) {
		t.Fatalf("imported session %s not discoverable under destDir", sessionID)
	}
	assertTranscriptCwd(t, destDir, sessionID, destDir)

	// Idempotent re-import: same id, still discoverable.
	reID, err := be.ImportSession(ctx, destDir, rewritten)
	if err != nil {
		t.Fatalf("re-ImportSession: %v", err)
	}
	if reID != sessionID {
		t.Errorf("re-import id = %q, want %q", reID, sessionID)
	}
}

// TestClaudeBackend_ImportPlacementOnly documents that DISCOVERY depends on
// file placement at the destination's encoded path, NOT on the in-file cwd
// rewrite: it imports an un-rewritten blob (cwd still the source path) and
// asserts the SDK still finds it. This pins "rewrite is not required for
// discovery" so the rewrite can be dropped later if it proves unnecessary
// (the rewrite is about cwd fidelity / permission prompts, verified in the
// manual round trip).
func TestClaudeBackend_ImportPlacementOnly(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	ctx := context.Background()
	const sessionID = "placement-9999"
	srcDir := t.TempDir()
	destDir := t.TempDir()

	blobPath := filepath.Join(t.TempDir(), "raw.jsonl")
	if err := os.WriteFile(blobPath, claudeTranscriptBytes(sessionID, srcDir), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (ClaudeBackend{}).ImportSession(ctx, destDir, blobPath); err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	infos, err := claudecode.ListSessions(claudecode.WithSessionDirectory(destDir))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if !containsSessionID(infos, sessionID) {
		t.Fatal("placement-only import not discoverable; the encoded PATH (not in-file cwd) is what discovery needs")
	}
}
