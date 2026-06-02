package sessionsync

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteClaudeImportBlob_RebasesCwd(t *testing.T) {
	t.Parallel()
	const sessionID = "rw-1"
	const srcCwd = "/laptop/src/worktree"
	const destDir = "/sandbox/work/wt-xyz"

	src := filepath.Join(t.TempDir(), "blob.jsonl")
	if err := os.WriteFile(src, claudeTranscriptBytes(sessionID, srcCwd), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := RewriteClaudeImportBlob(src, destDir)
	if err != nil {
		t.Fatalf("RewriteClaudeImportBlob: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(out) })

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rebased := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("rewritten line not JSON: %v", err)
		}
		if m["sessionId"] != sessionID {
			t.Errorf("sessionId = %v, want %q (preserved)", m["sessionId"], sessionID)
		}
		if cwd, ok := m["cwd"]; ok {
			rebased++
			if cwd != destDir {
				t.Errorf("cwd = %v, want %q (rebased)", cwd, destDir)
			}
			if m["gitBranch"] != "main" {
				t.Errorf("gitBranch = %v, want main (preserved untouched)", m["gitBranch"])
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if rebased != 2 {
		t.Errorf("rebased %d cwd lines, want 2 (the two message lines; metadata line has none)", rebased)
	}
}

// TestRewriteClaudeImportBlob_LargeLineSurvives proves the rewrite streams
// with bufio.Reader (not Scanner): a line far larger than Scanner's 64KB
// default token cap round-trips intact with its cwd rebased.
func TestRewriteClaudeImportBlob_LargeLineSurvives(t *testing.T) {
	t.Parallel()
	const sessionID = "rw-big"
	big := strings.Repeat("x", 256*1024)
	line := map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"cwd":       "/src",
		"message":   map[string]any{"role": "user", "content": big},
	}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(src, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := RewriteClaudeImportBlob(src, "/dest")
	if err != nil {
		t.Fatalf("RewriteClaudeImportBlob: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(out) })

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("rewritten not JSON (large line mangled?): %v", err)
	}
	if m["cwd"] != "/dest" {
		t.Errorf("cwd = %v, want /dest", m["cwd"])
	}
	msg, ok := m["message"].(map[string]any)
	if !ok {
		t.Fatalf("message field missing/!object after rewrite")
	}
	if msg["content"] != big {
		t.Errorf("large content not preserved (got len %d, want %d)", len(msg["content"].(string)), len(big))
	}
}
