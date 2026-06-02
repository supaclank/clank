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
			BlobKey:    "sessions/" + sessID + ".json",
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
