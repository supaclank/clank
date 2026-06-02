package sessionsync

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// claudeTranscriptBytes builds a small but SDK-valid Claude JSONL
// transcript for sessionID whose message lines sit under cwd. The first
// line is a metadata line (no cwd) so tests exercise per-line rewrite
// scoping.
func claudeTranscriptBytes(sessionID, cwd string) []byte {
	lines := []map[string]any{
		{"type": "ai-title", "aiTitle": "Test session", "sessionId": sessionID},
		{"type": "user", "sessionId": sessionID, "cwd": cwd, "gitBranch": "main", "uuid": "u-1", "timestamp": "2026-04-25T10:00:01Z", "message": map[string]any{"role": "user", "content": "hello"}},
		{"type": "assistant", "sessionId": sessionID, "cwd": cwd, "gitBranch": "main", "uuid": "a-1", "timestamp": "2026-04-25T10:00:02Z", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "hi"}}}},
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

// writeClaudeTranscript writes a transcript for sessionID under cwd's
// encoded project dir within the active CLAUDE_CONFIG_DIR, creating the
// project dir. Returns the transcript path.
func writeClaudeTranscript(t *testing.T, cwd, sessionID string) string {
	t.Helper()
	path := mustTranscriptPath(t, cwd, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(path, claudeTranscriptBytes(sessionID, cwd), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func mustTranscriptPath(t *testing.T, cwd, sessionID string) string {
	t.Helper()
	p, err := claudeTranscriptPath(cwd, sessionID)
	if err != nil {
		t.Fatalf("claudeTranscriptPath: %v", err)
	}
	return p
}

// assertTranscriptCwd asserts every transcript line that carries a cwd has
// it set to wantCwd.
func assertTranscriptCwd(t *testing.T, cwd, sessionID, wantCwd string) {
	t.Helper()
	data, err := os.ReadFile(mustTranscriptPath(t, cwd, sessionID))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if c, ok := m["cwd"]; ok && c != wantCwd {
			t.Errorf("transcript cwd = %v, want %q", c, wantCwd)
		}
	}
}

// appendLines appends raw JSONL lines (each gets a trailing newline) to an
// existing transcript — used to simulate `claude --resume` appending control
// lines or a new turn.
func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func containsSessionID(infos []claudecode.SDKSessionInfo, id string) bool {
	for _, i := range infos {
		if i.SessionID == id {
			return true
		}
	}
	return false
}
