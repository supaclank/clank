package sessionsync

import (
	"testing"
)

// TestClaudeSessionFingerprint_IgnoresControlAppends is the core regression
// test for the read-only-`--resume` drift bug: opening a session appends
// control lines (mode/ai-title/permission-mode) that bump the file mtime but
// add no real turn. The fingerprint must be unmoved by those, and must move
// only when a genuine uuid-bearing turn is appended.
//
// Not parallel: isolates CLAUDE_CONFIG_DIR via t.Setenv.
func TestClaudeSessionFingerprint_IgnoresControlAppends(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	cwd := t.TempDir()
	const id = "fp-sess-1"
	path := writeClaudeTranscript(t, cwd, id) // last uuid is "a-1"

	fp1 := claudeSessionFingerprint(cwd, id)
	if fp1 == "" {
		t.Fatal("fingerprint empty for a transcript that has uuid lines")
	}
	if fp1 != "a-1" {
		t.Fatalf("fingerprint = %q, want a-1 (last uuid in fixture)", fp1)
	}

	// Read-only `--resume` noise: control lines, no uuid.
	appendLines(t, path,
		`{"type":"ai-title","aiTitle":"x","sessionId":"`+id+`"}`,
		`{"type":"mode","mode":"normal","sessionId":"`+id+`"}`,
		`{"type":"permission-mode","permissionMode":"default","sessionId":"`+id+`"}`,
	)
	if fp2 := claudeSessionFingerprint(cwd, id); fp2 != fp1 {
		t.Errorf("fingerprint moved after control-line appends: %q -> %q", fp1, fp2)
	}

	// A genuine new turn carries a uuid and DOES move the fingerprint.
	appendLines(t, path, `{"type":"user","sessionId":"`+id+`","cwd":"`+cwd+`","uuid":"new-turn","message":{"role":"user","content":"more"}}`)
	fp3 := claudeSessionFingerprint(cwd, id)
	if fp3 == fp1 {
		t.Errorf("fingerprint did not move after a real new turn (still %q)", fp3)
	}
	if fp3 != "new-turn" {
		t.Errorf("fingerprint = %q, want new-turn", fp3)
	}
}

func TestClaudeSessionFingerprint_MissingTranscript(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	if fp := claudeSessionFingerprint(t.TempDir(), "does-not-exist"); fp != "" {
		t.Errorf("fingerprint = %q, want empty for a missing transcript", fp)
	}
}
