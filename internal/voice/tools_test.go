package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// stubToolProvider implements ToolProvider for testing. Only the methods
// needed by the tests are filled in — everything else panics so we catch
// unexpected calls immediately.
type stubToolProvider struct {
	knownDirs      []string
	knownDirsErr   error
	createdSession *agent.SessionInfo
	createErr      error
	sessions       []agent.SessionInfo
}

func (s *stubToolProvider) SearchSessions(_ context.Context, _ agent.SearchParams) ([]agent.SessionInfo, error) {
	return s.sessions, nil
}
func (s *stubToolProvider) GetSession(_ context.Context, id string) (*agent.SessionInfo, error) {
	for i := range s.sessions {
		if s.sessions[i].ID == id {
			return &s.sessions[i], nil
		}
	}
	return nil, fmt.Errorf("not found: %s", id)
}
func (s *stubToolProvider) GetSessionMessages(context.Context, string) ([]agent.MessageData, error) {
	panic("unexpected call")
}
func (s *stubToolProvider) SendMessage(context.Context, string, string) error {
	panic("unexpected call")
}
func (s *stubToolProvider) CreateSession(_ context.Context, req agent.StartRequest) (*agent.SessionInfo, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return s.createdSession, nil
}
func (s *stubToolProvider) AbortSession(context.Context, string) error {
	panic("unexpected call")
}
func (s *stubToolProvider) KnownProjectDirs(context.Context) ([]string, error) {
	return s.knownDirs, s.knownDirsErr
}

func TestBuildInstructions(t *testing.T) {
	t.Parallel()

	t.Run("no known dirs", func(t *testing.T) {
		t.Parallel()
		instructions := buildInstructions(nil)
		if !strings.Contains(instructions, "No known projects yet") {
			t.Error("expected 'No known projects yet' message for empty dirs")
		}
		if !strings.Contains(instructions, baseInstructions) {
			t.Error("expected base instructions to be present")
		}
	})

	t.Run("with known dirs", func(t *testing.T) {
		t.Parallel()
		dirs := []string{"/home/user/projects/foo", "/home/user/projects/bar"}
		instructions := buildInstructions(dirs)

		if !strings.Contains(instructions, baseInstructions) {
			t.Error("expected base instructions to be present")
		}
		if !strings.Contains(instructions, "Known projects:") {
			t.Error("expected 'Known projects:' header")
		}
		if !strings.Contains(instructions, "/home/user/projects/foo (foo)") {
			t.Errorf("expected full path with base name for foo, got:\n%s", instructions)
		}
		if !strings.Contains(instructions, "/home/user/projects/bar (bar)") {
			t.Errorf("expected full path with base name for bar, got:\n%s", instructions)
		}
	})
}

func TestValidateKnownProjectDir(t *testing.T) {
	t.Parallel()

	t.Run("valid dir", func(t *testing.T) {
		t.Parallel()
		tp := &stubToolProvider{
			knownDirs: []string{"/home/user/projects/foo", "/home/user/projects/bar"},
		}
		err := validateKnownProjectDir(tp, "/home/user/projects/foo")
		if err != nil {
			t.Errorf("expected no error for known dir, got: %v", err)
		}
	})

	t.Run("unknown dir", func(t *testing.T) {
		t.Parallel()
		tp := &stubToolProvider{
			knownDirs: []string{"/home/user/projects/foo", "/home/user/projects/bar"},
		}
		err := validateKnownProjectDir(tp, "/github.com/wrong/path")
		if err == nil {
			t.Fatal("expected error for unknown dir")
		}
		if !strings.Contains(err.Error(), "is not a known project directory") {
			t.Errorf("expected 'not a known project directory' in error, got: %v", err)
		}
		// Error should list valid directories to help the agent self-correct.
		if !strings.Contains(err.Error(), "/home/user/projects/foo") {
			t.Errorf("expected valid dir in error message, got: %v", err)
		}
	})

	t.Run("no known dirs", func(t *testing.T) {
		t.Parallel()
		tp := &stubToolProvider{knownDirs: nil}
		err := validateKnownProjectDir(tp, "/some/path")
		if err == nil {
			t.Fatal("expected error when no known dirs exist")
		}
		if !strings.Contains(err.Error(), "no known projects exist") {
			t.Errorf("expected 'no known projects exist' in error, got: %v", err)
		}
	})

	t.Run("lookup error propagates", func(t *testing.T) {
		t.Parallel()
		tp := &stubToolProvider{knownDirsErr: fmt.Errorf("db down")}
		err := validateKnownProjectDir(tp, "/some/path")
		if err == nil {
			t.Fatal("expected error when lookup fails")
		}
		if !strings.Contains(err.Error(), "db down") {
			t.Errorf("expected underlying error, got: %v", err)
		}
	})
}

func TestCreateSessionTool_RejectsUnknownDir(t *testing.T) {
	t.Parallel()

	tp := &stubToolProvider{
		knownDirs: []string{"/home/user/projects/clank"},
	}
	tool := createSessionTool(tp)

	input, _ := json.Marshal(map[string]string{
		"backend":     "opencode",
		"project_dir": "/github.com/wrong/path",
		"prompt":      "fix the bug",
	})

	result, err := tool.Fn(input)
	if err == nil {
		t.Fatalf("expected error for unknown dir, got result: %s", result)
	}
	if !strings.Contains(err.Error(), "is not a known project directory") {
		t.Errorf("expected known-dir validation error, got: %v", err)
	}
}

func TestCreateSessionTool_AcceptsKnownDir(t *testing.T) {
	t.Parallel()

	tp := &stubToolProvider{
		knownDirs: []string{"/home/user/projects/clank"},
		createdSession: &agent.SessionInfo{
			ID:          "01ABCDEF01ABCDEF01ABCDEF01",
			ProjectName: "clank",
			ProjectDir:  "/home/user/projects/clank",
		},
	}
	tool := createSessionTool(tp)

	input, _ := json.Marshal(map[string]string{
		"backend":     "opencode",
		"project_dir": "/home/user/projects/clank",
		"prompt":      "fix the bug",
	})

	result, err := tool.Fn(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Session created") {
		t.Errorf("expected 'Session created' in result, got: %s", result)
	}
}

func TestListSessionsTool_IncludesProjectDir(t *testing.T) {
	t.Parallel()

	tp := &stubToolProvider{
		sessions: []agent.SessionInfo{
			{
				ID:          "01ABCDEF01ABCDEF01ABCDEF01",
				Status:      agent.StatusBusy,
				Backend:     agent.BackendOpenCode,
				ProjectDir:  "/home/user/projects/clank",
				ProjectName: "clank",
				Prompt:      "fix the bug",
			},
		},
	}
	tool := listSessionsTool(tp)

	input, _ := json.Marshal(map[string]string{})
	result, err := tool.Fn(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should contain full project dir, not just project name.
	if !strings.Contains(result, "/home/user/projects/clank") {
		t.Errorf("expected full project dir in output, got: %s", result)
	}
}

func TestListSessionsTool_OutputsFullSessionID(t *testing.T) {
	t.Parallel()

	fullID := "01ABCDEF01ABCDEF01ABCDEF01"
	tp := &stubToolProvider{
		sessions: []agent.SessionInfo{
			{
				ID:      fullID,
				Status:  agent.StatusBusy,
				Backend: agent.BackendOpenCode,
				Prompt:  "do stuff",
			},
		},
	}
	tool := listSessionsTool(tp)
	input, _ := json.Marshal(map[string]string{})
	result, err := tool.Fn(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, fullID) {
		t.Errorf("list_sessions should output full session ID %q, got: %s", fullID, result)
	}
}

func TestResolveSessionID(t *testing.T) {
	t.Parallel()

	sessions := []agent.SessionInfo{
		{ID: "01AAAA0000AAAA0000AAAA0001"},
		{ID: "01BBBB0000BBBB0000BBBB0001"},
	}
	tp := &stubToolProvider{sessions: sessions}

	t.Run("exact match", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		got, err := resolveSessionID(ctx, tp, "01AAAA0000AAAA0000AAAA0001")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "01AAAA0000AAAA0000AAAA0001" {
			t.Errorf("got %q, want %q", got, "01AAAA0000AAAA0000AAAA0001")
		}
	})

	t.Run("partial id rejected", func(t *testing.T) {
		t.Parallel()
		// Regression: voice agent was passing truncated 8-char IDs from
		// list_sessions output. Now list_sessions outputs full IDs, and
		// resolveSessionID requires an exact match.
		ctx := context.Background()
		_, err := resolveSessionID(ctx, tp, "01BBBB00")
		if err == nil {
			t.Fatal("expected error for partial ID")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("expected 'not found' in error, got: %v", err)
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := resolveSessionID(ctx, tp, "ZZZZNOTFOUND0000000000000000")
		if err == nil {
			t.Fatal("expected error for no match")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("expected 'not found' in error, got: %v", err)
		}
	})

	t.Run("empty id", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := resolveSessionID(ctx, tp, "")
		if err == nil {
			t.Fatal("expected error for empty id")
		}
		if !strings.Contains(err.Error(), "session_id is required") {
			t.Errorf("expected 'session_id is required' in error, got: %v", err)
		}
	})
}
