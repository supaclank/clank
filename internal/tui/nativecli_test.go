package tui

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
)

func TestNativeCLICmd_OpenCode(t *testing.T) {
	t.Parallel()

	info := &agent.SessionInfo{
		ID:         "ses-123",
		ExternalID: "oc-ext-456",
		Backend:    agent.BackendOpenCode,
		ServerURL:  "http://127.0.0.1:4123",
	}

	cmd, err := nativeCLICmd(info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := cmd.Args
	want := []string{"opencode", "attach", "http://127.0.0.1:4123", "--session", "oc-ext-456"}
	if len(args) != len(want) {
		t.Fatalf("args length = %d, want %d\ngot:  %v\nwant: %v", len(args), len(want), args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestNativeCLICmd_MissingExternalID(t *testing.T) {
	t.Parallel()

	info := &agent.SessionInfo{
		ID:        "ses-123",
		Backend:   agent.BackendOpenCode,
		ServerURL: "http://127.0.0.1:4123",
	}

	_, err := nativeCLICmd(info)
	if err == nil {
		t.Fatal("expected error for missing ExternalID")
	}
}

func TestNativeCLICmd_MissingServerURL(t *testing.T) {
	t.Parallel()

	info := &agent.SessionInfo{
		ID:         "ses-123",
		ExternalID: "oc-ext-456",
		Backend:    agent.BackendOpenCode,
	}

	_, err := nativeCLICmd(info)
	if err == nil {
		t.Fatal("expected error for missing ServerURL")
	}
}

func TestNativeCLICmd_Claude(t *testing.T) {
	t.Parallel()

	info := &agent.SessionInfo{
		ID:         "ses-123",
		ExternalID: "claude-ext-789",
		Backend:    agent.BackendClaudeCode,
		GitRef:     agent.GitRef{LocalPath: "/repo/clank"},
	}

	cmd, err := nativeCLICmd(info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"claude", "--resume", "claude-ext-789"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args length = %d, want %d\ngot:  %v\nwant: %v", len(cmd.Args), len(want), cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
	if cmd.Dir != "/repo/clank" {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, "/repo/clank")
	}
}

func TestNativeCLICmd_Claude_MissingExternalID(t *testing.T) {
	t.Parallel()

	info := &agent.SessionInfo{
		ID:      "ses-123",
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{LocalPath: "/repo/clank"},
	}

	_, err := nativeCLICmd(info)
	if err == nil {
		t.Fatal("expected error for missing ExternalID")
	}
}

func TestNativeCLICmd_Claude_MissingLocalPath(t *testing.T) {
	t.Parallel()

	info := &agent.SessionInfo{
		ID:         "ses-123",
		ExternalID: "claude-ext-789",
		Backend:    agent.BackendClaudeCode,
	}

	_, err := nativeCLICmd(info)
	if err == nil {
		t.Fatal("expected error for missing LocalPath")
	}
}

func TestNativeCLICmd_UnsupportedBackend(t *testing.T) {
	t.Parallel()

	info := &agent.SessionInfo{
		ID:         "ses-123",
		ExternalID: "ext-id",
		Backend:    agent.BackendType("not-a-real-backend"),
	}

	_, err := nativeCLICmd(info)
	if err == nil {
		t.Fatal("expected error for unsupported backend")
	}
}

func TestNativeCLICmd_NilInfo(t *testing.T) {
	t.Parallel()

	_, err := nativeCLICmd(nil)
	if err == nil {
		t.Fatal("expected error for nil info")
	}
}

func TestOpenNativeCLI_ErrorReturnsMsg(t *testing.T) {
	t.Parallel()

	// Unknown backend — should return an error message immediately.
	info := &agent.SessionInfo{
		ID:      "ses-123",
		Backend: agent.BackendType("not-a-real-backend"),
	}

	cmd := openNativeCLI(info)
	if cmd == nil {
		t.Fatal("expected non-nil Cmd")
	}

	// Execute the Cmd to get the message.
	msg := cmd()
	ret, ok := msg.(nativeCLIReturnMsg)
	if !ok {
		t.Fatalf("expected nativeCLIReturnMsg, got %T", msg)
	}
	if ret.err == nil {
		t.Error("expected error in return message")
	}
	if ret.sessionID != "ses-123" {
		t.Errorf("sessionID = %q, want %q", ret.sessionID, "ses-123")
	}
}
