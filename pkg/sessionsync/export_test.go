package sessionsync

import (
	"context"
	"reflect"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// exportProgressRecorder captures the SessionProgress (done,total) sequence
// emitted during export. The other PushObserver methods are unused here.
type exportProgressRecorder struct{ calls [][2]int }

func (r *exportProgressRecorder) Phase(string)             {}
func (r *exportProgressRecorder) UploadSized(int64)        {}
func (r *exportProgressRecorder) UploadProgress(int64)     {}
func (r *exportProgressRecorder) SessionProgress(d, t int) { r.calls = append(r.calls, [2]int{d, t}) }

// TestPreparePush_DiscoversClaude proves Claude sessions are exported even
// when the opencode binary is absent (PATH stripped) — the missing-binary
// case no longer short-circuits the whole export, and the SessionEntry
// carries the Claude backend + the session id as both SessionID and
// ExternalID (daemon-free has no separate clank ULID). Against an empty
// record every session is unsynced, so the changed-set is the full set.
//
// Not parallel: isolates CLAUDE_CONFIG_DIR and PATH via t.Setenv.
func TestPreparePush_DiscoversClaude(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no opencode (and no git) on PATH
	ctx := context.Background()

	wtDir := t.TempDir()
	const sessionID = "export-wt-claude-7777"
	writeClaudeTranscript(t, wtDir, sessionID)

	obs := &exportProgressRecorder{}
	plan, err := PreparePush(ctx, wtDir, agent.SyncedSessionRecord{Sessions: map[string]agent.SyncedSession{}}, obs)
	if err != nil {
		t.Fatalf("PreparePush: %v", err)
	}
	t.Cleanup(plan.Cleanup)

	// Export must report (i/N): a 0/1 kickoff then 1/1 once the blob lands,
	// so the push UI can show live progress through the slow local copy.
	if want := [][2]int{{0, 1}, {1, 1}}; !reflect.DeepEqual(obs.calls, want) {
		t.Errorf("session progress calls = %v, want %v", obs.calls, want)
	}

	if len(plan.Upload) != 1 {
		t.Fatalf("want 1 exported claude session, got %d (opencode-absent must not hide Claude)", len(plan.Upload))
	}
	e := plan.Upload[0].Entry
	if e.Backend != agent.BackendClaudeCode {
		t.Errorf("Backend = %q, want claude-code", e.Backend)
	}
	if e.ExternalID != sessionID {
		t.Errorf("ExternalID = %q, want %q", e.ExternalID, sessionID)
	}
	if e.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q (= ExternalID in daemon-free mode)", e.SessionID, sessionID)
	}
	if e.Bytes <= 0 {
		t.Errorf("Bytes = %d, want positive", e.Bytes)
	}
	if e.ContentHash == "" {
		t.Error("ContentHash must be set on an exported entry")
	}
	// The content fingerprint (last-message uuid) is carried in the record so
	// later drift detection ignores mtime-only bumps; the complete manifest
	// references the session too.
	if got := plan.Record.Sessions[sessionID].Fingerprint; got != "a-1" {
		t.Errorf("Record fingerprint = %q, want a-1 (last uuid in the fixture)", got)
	}
	if len(plan.Entries) != 1 {
		t.Errorf("complete manifest should list the session, got %d entries", len(plan.Entries))
	}
}
