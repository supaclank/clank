package sessionsync

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// opencode prints empty stdout (not "[]") when there are no sessions. That
// must decode to zero sessions, not abort the whole push with "unexpected end
// of JSON input". Regression for `clank push` failing on a machine that has no
// opencode sessions.
func TestParseOpenCodeSessionList_EmptyOutputYieldsNoSessions(t *testing.T) {
	t.Parallel()
	for _, in := range [][]byte{nil, {}, []byte("\n"), []byte("  \n\t ")} {
		got, err := parseOpenCodeSessionList(in)
		if err != nil {
			t.Fatalf("parseOpenCodeSessionList(%q): unexpected error: %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("parseOpenCodeSessionList(%q): want 0 sessions, got %d", in, len(got))
		}
	}
}

func TestParseOpenCodeSessionList_DecodesEntries(t *testing.T) {
	t.Parallel()
	const raw = `[
  {"id":"ses_a","title":"First","updated":1780487826605,"created":1780487822576,"projectId":"p","directory":"/work/a"},
  {"id":"ses_b","title":"Second","updated":1780155547689,"created":1780155354742,"projectId":"p","directory":"/work/b"}
]`
	got, err := parseOpenCodeSessionList([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got))
	}
	if got[0].Backend != agent.BackendOpenCode {
		t.Fatalf("entry 0 backend: want %q, got %q", agent.BackendOpenCode, got[0].Backend)
	}
	if got[0].ExternalID != "ses_a" || got[0].Title != "First" || got[0].ProjectDir != "/work/a" {
		t.Fatalf("entry 0 mismatch: %+v", got[0])
	}
	if got[1].ExternalID != "ses_b" {
		t.Fatalf("entry 1 mismatch: %+v", got[1])
	}
}

// Genuinely malformed output (not merely empty) must still fail loudly so real
// corruption isn't silently swallowed as "no sessions".
func TestParseOpenCodeSessionList_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	if _, err := parseOpenCodeSessionList([]byte("{")); err == nil {
		t.Fatal("want error for malformed json, got nil")
	}
}
