package sessionsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// TestImportWorktreeSessions_RoundTrip serves a session manifest + blob
// over httptest and imports it into a fresh worktree dir, asserting the
// session lands under that dir (directory rebased) with its external id
// preserved. Real opencode; isolated HOME (not parallel — t.Setenv).
func TestImportWorktreeSessions_RoundTrip(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}
	home := t.TempDir()
	mustMkdirT(t, filepath.Join(home, ".local/share/opencode"))
	mustMkdirT(t, filepath.Join(home, ".config/opencode"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	ctx := context.Background()
	const sessID = "ses_import00000000000000000000"
	// The blob's original directory is a source-host path that doesn't
	// exist here; RewriteImportBlob rebases it to destDir on import.
	blob := ocTestBlob(sessID, "/some/source/host/path")

	manifest := checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: "ckpt-test",
		Sessions: []checkpoint.SessionEntry{{
			SessionID:  sessID,
			ExternalID: sessID,
			Backend:    agent.BackendOpenCode,
		}},
	}
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest":
			_, _ = w.Write(manifestBytes)
		case "/blob":
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	destDir := t.TempDir()
	imported, err := ImportWorktreeSessions(ctx, srv.Client(), srv.URL+"/manifest", map[string]string{sessID: srv.URL + "/blob"}, destDir)
	if err != nil {
		t.Fatalf("ImportWorktreeSessions: %v", err)
	}
	if len(imported) != 1 || imported[0] != sessID {
		t.Fatalf("imported = %v, want [%s] (opencode preserves info.id)", imported, sessID)
	}

	scoped, err := (OpenCodeBackend{}).ListSessions(ctx, destDir)
	if err != nil {
		t.Fatalf("ListSessions(destDir): %v", err)
	}
	if _, ok := dirForExternalID(scoped, sessID); !ok {
		t.Fatalf("imported session %s not listed under destDir %s", sessID, destDir)
	}
}

// TestImportWorktreeSessions_Claude serves a Claude session manifest + JSONL
// blob over httptest and imports it into a fresh worktree dir, asserting the
// transcript lands under that dir (discoverable by the SDK, cwd rebased)
// with its session id preserved. No claude binary needed.
//
// Not parallel: isolates CLAUDE_CONFIG_DIR via t.Setenv.
func TestImportWorktreeSessions_Claude(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	ctx := context.Background()
	const sessionID = "import-wt-claude-0001"
	// The blob's cwd is a source-host path; import rebases it to destDir.
	blob := claudeTranscriptBytes(sessionID, "/laptop/source/path")

	manifest := checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: "ckpt-claude",
		Sessions: []checkpoint.SessionEntry{{
			SessionID:  sessionID,
			ExternalID: sessionID,
			Backend:    agent.BackendClaudeCode,
		}},
	}
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest":
			_, _ = w.Write(manifestBytes)
		case "/blob":
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	destDir := t.TempDir()
	imported, err := ImportWorktreeSessions(ctx, srv.Client(), srv.URL+"/manifest", map[string]string{sessionID: srv.URL + "/blob"}, destDir)
	if err != nil {
		t.Fatalf("ImportWorktreeSessions: %v", err)
	}
	if len(imported) != 1 || imported[0] != sessionID {
		t.Fatalf("imported = %v, want [%s] (Claude preserves the session id)", imported, sessionID)
	}

	infos, err := claudecode.ListSessions(claudecode.WithSessionDirectory(destDir))
	if err != nil {
		t.Fatalf("ListSessions(destDir): %v", err)
	}
	if !containsSessionID(infos, sessionID) {
		t.Fatalf("imported claude session %s not discoverable under destDir %s", sessionID, destDir)
	}
	assertTranscriptCwd(t, destDir, sessionID, destDir)
}
