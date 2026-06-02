package sessionsync

import (
	"testing"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

func TestEncodeClaudeCwd(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/Users/me/repo": "-Users-me-repo",
		"/a/b.c/d-e":     "-a-b-c-d-e",
		"relative/path":  "relative-path",
		"":               "",
		"/x/.claude/wt":  "-x--claude-wt",
	}
	for in, want := range cases {
		if got := encodeClaudeCwd(in); got != want {
			t.Errorf("encodeClaudeCwd(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClaudeTranscriptPath_AnchoredToSDK is the load-bearing guard for
// keeping the cwd-encoding in this repo (rather than the SDK): it writes a
// transcript at the path WE compute, then asserts the SDK's own discovery
// finds it. If our encoding ever drifts from the SDK's, this fails in CI.
//
// Not parallel: isolates CLAUDE_CONFIG_DIR via t.Setenv.
func TestClaudeTranscriptPath_AnchoredToSDK(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	dir := t.TempDir()
	const sessionID = "anchored-1111-2222-3333"
	writeClaudeTranscript(t, dir, sessionID)

	infos, err := claudecode.ListSessions(claudecode.WithSessionDirectory(dir))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if !containsSessionID(infos, sessionID) {
		t.Fatalf("SDK did not discover %s under %s — our cwd-encoding has drifted from the SDK's", sessionID, dir)
	}
}
