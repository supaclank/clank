package sessionsync

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOpenCodeBackend_RoundTrip exercises the daemon-free opencode
// Backend against a real opencode binary in an isolated HOME: import a
// synthetic session, discover it (global + worktree-scoped), export it,
// and re-import (idempotent). Hermetic — no network/credentials.
//
// Not parallel: it isolates opencode storage via t.Setenv, which
// mutates process env.
func TestOpenCodeBackend_RoundTrip(t *testing.T) {
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
	var be OpenCodeBackend

	wtDir := t.TempDir()
	const sessID = "ses_roundtrip00000000000000000"
	blobPath := filepath.Join(t.TempDir(), "blob.json")
	if err := os.WriteFile(blobPath, ocTestBlob(sessID, wtDir), 0o644); err != nil {
		t.Fatal(err)
	}

	gotID, err := be.ImportSession(ctx, "", blobPath, "")
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if gotID != sessID {
		t.Fatalf("imported id = %q, want %q (opencode preserves info.id)", gotID, sessID)
	}

	// Global list contains it; capture the directory opencode reports
	// (it may normalize the path) to drive the scoping assertions.
	all, err := be.ListSessions(ctx, "")
	if err != nil {
		t.Fatalf("ListSessions(all): %v", err)
	}
	reportedDir, ok := dirForExternalID(all, sessID)
	if !ok {
		t.Fatalf("global list missing %s; got %d sessions", sessID, len(all))
	}

	// Scoped to the session's own directory → present; an unrelated
	// directory → absent (this is the samePath filter).
	scoped, err := be.ListSessions(ctx, reportedDir)
	if err != nil {
		t.Fatalf("ListSessions(%s): %v", reportedDir, err)
	}
	if _, ok := dirForExternalID(scoped, sessID); !ok {
		t.Fatalf("scoped list to %s missing %s", reportedDir, sessID)
	}
	other, err := be.ListSessions(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dirForExternalID(other, sessID); ok {
		t.Fatal("session leaked into an unrelated directory scope")
	}

	// Export → re-import preserves the id (additive-merge, idempotent).
	var buf bytes.Buffer
	if err := be.ExportSession(ctx, "", sessID, &buf); err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(buf.Bytes()), []byte("{")) {
		t.Fatalf("export not JSON: %.60q", buf.String())
	}
	exportPath := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(exportPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	reID, err := be.ImportSession(ctx, "", exportPath, "")
	if err != nil {
		t.Fatalf("re-ImportSession: %v", err)
	}
	if reID != sessID {
		t.Fatalf("re-import id = %q, want %q", reID, sessID)
	}
}

func dirForExternalID(sessions []DiscoveredSession, id string) (string, bool) {
	for _, s := range sessions {
		if s.ExternalID == id {
			return s.ProjectDir, true
		}
	}
	return "", false
}

func mustMkdirT(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// ocTestBlob builds a minimal opencode export blob (one message) filed
// under dir, mirroring the schema in internal/agent's import test.
func ocTestBlob(id, dir string) []byte {
	const msgID = "msg_roundtrip0000000000000000"
	body := map[string]any{
		"info": map[string]any{
			"id":        id,
			"slug":      "clank-sessionsync-test",
			"projectID": "0000000000000000000000000000000000000000",
			"directory": dir,
			"title":     "roundtrip",
			"version":   "1.3.15",
			"summary":   map[string]any{"additions": 0, "deletions": 0, "files": 0},
			"time":      map[string]any{"created": 1000, "updated": 1000},
		},
		"messages": []map[string]any{
			{
				"info": map[string]any{
					"id":        msgID,
					"sessionID": id,
					"role":      "user",
					"agent":     "build",
					"model":     map[string]any{"providerID": "diag", "modelID": "diag"},
					"summary":   map[string]any{"diffs": []any{}},
					"time":      map[string]any{"created": 1000},
				},
				"parts": []map[string]any{
					{
						"type":      "text",
						"text":      "hello",
						"id":        "prt_" + msgID,
						"sessionID": id,
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
