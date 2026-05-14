package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// TestOpenCodeImportSemantics empirically pins the opencode CLI behaviors
// that the clank session-sync feature depends on. Hermetic — uses an
// isolated HOME and synthetic blobs, so no LLM calls, no credentials,
// no network. Skips if `opencode` is not on $PATH.
//
// Confirmed semantics (opencode 1.3.15) — if any assertion fails, STOP
// and revisit ~/.claude/plans/i-want-to-plan-moonlit-mountain.md §E:
//
//  1. `opencode export <id>` writes a `Exporting session: <id>` prefix
//     to STDERR; the JSON blob goes to STDOUT. So the export wrapper
//     must read stdout (not stdout+stderr).
//  2. `opencode import <file>` PRESERVES `info.id` from the blob. The
//     imported session is listable under the original ID.
//  3. `opencode import` is ADDITIVE MERGE keyed by message ID. Messages
//     in the blob are upserted; messages on disk but absent from the
//     blob are KEPT (not deleted). Safe for clank's exclusive-ownership
//     migration model: while a worktree is owned remote, the laptop
//     cannot append divergent messages, so the merge is well-defined.
//  4. `opencode import` is idempotent — re-importing the same blob
//     succeeds, no duplicate session.
//  5. CLI ops (import/export/session list/delete) operate purely on
//     local storage. No auth/credentials/network needed.
func TestOpenCodeImportSemantics(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, ".local/share/opencode"))
	mustMkdir(t, filepath.Join(home, ".config/opencode"))
	env := append(os.Environ(),
		"HOME="+home,
		"XDG_DATA_HOME="+filepath.Join(home, ".local/share"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)

	const sessID = "ses_diagsemantics0000000000000"

	// --- §1 + §2: import preserves info.id; export prefix is stderr ---
	msgA := "msg_diagA0000000000000000000000"
	blob1 := writeOCBlob(t, ocBlob(sessID, []ocMsg{{ID: msgA, Role: "user", Text: "hello A"}}))
	runOC(t, env, "import", blob1)

	if !ocListContains(t, env, sessID) {
		t.Fatalf("§E (a) violated: after import, %s not in session list", sessID)
	}

	stdout, stderr := runOCBoth(t, env, "export", sessID)
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("export stdout should start with JSON {, got: %.80q", stdout)
	}
	if !strings.Contains(stderr, "Exporting session: "+sessID) {
		t.Errorf("export stderr should contain 'Exporting session: %s', got: %q", sessID, stderr)
	}
	exp1 := parseOCExport(t, []byte(stdout))
	if exp1.Info.ID != sessID {
		t.Errorf("§E (a) violated: exported info.id=%q want %q", exp1.Info.ID, sessID)
	}
	if len(exp1.Messages) != 1 {
		t.Fatalf("after import of 1-msg blob, want 1 message, got %d", len(exp1.Messages))
	}

	// --- §3: import-over-existing-with-extra → additive merge (count grows) ---
	msgB := "msg_diagB0000000000000000000000"
	blob2 := writeOCBlob(t, ocBlob(sessID, []ocMsg{
		{ID: msgA, Role: "user", Text: "hello A"},
		{ID: msgB, Role: "user", Text: "hello B"},
	}))
	runOC(t, env, "import", blob2)
	exp2 := ocExport(t, env, sessID)
	if len(exp2.Messages) != 2 {
		t.Fatalf("after import of 2-msg blob, want 2 messages, got %d", len(exp2.Messages))
	}

	// --- §3 cont.: re-import the 1-msg blob; B should be RETAINED (additive merge) ---
	runOC(t, env, "import", blob1)
	exp3 := ocExport(t, env, sessID)
	ids3 := ocMessageIDs(exp3)
	if !containsStr(ids3, msgB) {
		t.Errorf("§E additive-merge violated: re-import of 1-msg blob removed msgB. ids=%v", ids3)
	}
	if len(exp3.Messages) != 2 {
		t.Errorf("§E additive-merge violated: want 2 messages after re-import of subset blob, got %d", len(exp3.Messages))
	}

	// --- §2 + §4: delete + import preserves the original ID idempotently ---
	runOC(t, env, "session", "delete", sessID)
	if ocListContains(t, env, sessID) {
		t.Fatalf("session delete did not remove %s", sessID)
	}
	runOC(t, env, "import", blob1)
	if !ocListContains(t, env, sessID) {
		t.Errorf("after delete + import, session %s not listed under preserved ID", sessID)
	}
}

type ocMsg struct {
	ID, Role, Text string
}

type ocSessionInfo struct {
	Info struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"info"`
	Messages []json.RawMessage `json:"messages"`
}

func ocBlob(id string, msgs []ocMsg) []byte {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"info": map[string]any{
				"id":        m.ID,
				"sessionID": id,
				"role":      m.Role,
				"agent":     "build",
				"model":     map[string]any{"providerID": "diag", "modelID": "diag"},
				"summary":   map[string]any{"diffs": []any{}},
				"time":      map[string]any{"created": 1000},
			},
			"parts": []map[string]any{
				{
					"type":      "text",
					"text":      m.Text,
					"id":        "prt_" + m.ID,
					"sessionID": id,
					"messageID": m.ID,
				},
			},
		})
	}
	body := map[string]any{
		"info": map[string]any{
			"id":        id,
			"slug":      "diag-slug",
			"projectID": "0000000000000000000000000000000000000000",
			"directory": "/tmp/clank-diag-test",
			"title":     "diag",
			"version":   "1.3.15",
			"summary":   map[string]any{"additions": 0, "deletions": 0, "files": 0},
			"time":      map[string]any{"created": 1000, "updated": 1000},
		},
		"messages": out,
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return b
}

func writeOCBlob(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runOC(t *testing.T, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("opencode", args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("opencode %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func runOCBoth(t *testing.T, env []string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command("opencode", args...)
	cmd.Env = env
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("opencode %s: %v\nstderr: %s", strings.Join(args, " "), err, errBuf.String())
	}
	return outBuf.String(), errBuf.String()
}

func ocExport(t *testing.T, env []string, id string) ocSessionInfo {
	t.Helper()
	cmd := exec.Command("opencode", "export", id)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("opencode export %s: %v", id, err)
	}
	return parseOCExport(t, out)
}

func parseOCExport(t *testing.T, data []byte) ocSessionInfo {
	t.Helper()
	var s ocSessionInfo
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse export JSON: %v\n%s", err, string(data))
	}
	return s
}

func ocMessageIDs(s ocSessionInfo) []string {
	out := make([]string, 0, len(s.Messages))
	for _, m := range s.Messages {
		var v struct {
			Info struct {
				ID string `json:"id"`
			} `json:"info"`
		}
		if err := json.Unmarshal(m, &v); err == nil {
			out = append(out, v.Info.ID)
		}
	}
	return out
}

func ocListContains(t *testing.T, env []string, id string) bool {
	t.Helper()
	cmd := exec.Command("opencode", "session", "list")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("opencode session list: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == id {
			return true
		}
	}
	return false
}

func containsStr(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestOpenCodeExportImport_RoundTrip exercises the Go wrappers
// (OpenCodeExportSession + OpenCodeImportSession) against a real
// opencode binary in an isolated HOME. Confirms:
//
//   - Export writes JSON to the supplied io.Writer (stderr prefix
//     is properly discarded).
//   - Import returns the preserved info.id from the blob.
//   - Round-trip is value-preserving.
func TestOpenCodeExportImport_RoundTrip(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, ".local/share/opencode"))
	mustMkdir(t, filepath.Join(home, ".config/opencode"))

	// Override the env in-process so the subprocess inherits these
	// HOME/XDG paths — the wrapper functions don't take an env, by
	// design (they inherit from clank-host's environment).
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	const sessID = "ses_diagroundtrip000000000000000"

	// Seed: import an initial blob so a session exists for export.
	seedBlob := writeOCBlob(t, ocBlob(sessID, []ocMsg{{ID: "msg_seed00000000000000000000000", Role: "user", Text: "seed"}}))
	importedID, err := agent.OpenCodeImportSession(context.Background(), "", seedBlob)
	if err != nil {
		t.Fatalf("OpenCodeImportSession (seed): %v", err)
	}
	if importedID != sessID {
		t.Errorf("seed import: returned external_id %q, want %q", importedID, sessID)
	}

	// Round-trip: export -> import.
	var buf bytes.Buffer
	if err := agent.OpenCodeExportSession(context.Background(), "", sessID, &buf); err != nil {
		t.Fatalf("OpenCodeExportSession: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("export produced empty output")
	}
	var info struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(buf.Bytes(), &info); err != nil {
		t.Fatalf("export output is not valid JSON: %v\n%s", err, buf.String())
	}
	if info.Info.ID != sessID {
		t.Errorf("exported info.id = %q, want %q", info.Info.ID, sessID)
	}

	// Re-import the exported blob.
	roundTripPath := writeOCBlob(t, buf.Bytes())
	id2, err := agent.OpenCodeImportSession(context.Background(), "", roundTripPath)
	if err != nil {
		t.Fatalf("OpenCodeImportSession (round-trip): %v", err)
	}
	if id2 != sessID {
		t.Errorf("round-trip import: returned external_id %q, want %q", id2, sessID)
	}
}

// TestOpenCodeExport_EmptyExternalIDRejected — pinned input validation.
func TestOpenCodeExport_EmptyExternalIDRejected(t *testing.T) {
	t.Parallel()
	err := agent.OpenCodeExportSession(context.Background(), "", "", new(bytes.Buffer))
	if err == nil {
		t.Fatal("expected error for empty externalID")
	}
	if !strings.Contains(err.Error(), "externalID is required") {
		t.Errorf("expected 'externalID is required' error, got: %v", err)
	}
}

// TestOpenCodeImport_EmptyBlobPathRejected — pinned input validation.
func TestOpenCodeImport_EmptyBlobPathRejected(t *testing.T) {
	t.Parallel()
	_, err := agent.OpenCodeImportSession(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error for empty blobPath")
	}
	if !strings.Contains(err.Error(), "blobPath is required") {
		t.Errorf("expected 'blobPath is required' error, got: %v", err)
	}
}
