package agent

import (
	"testing"

	codex "github.com/pmenglund/codex-sdk-go"
	"github.com/pmenglund/codex-sdk-go/protocol"
)

func TestCodexTurnPolicyMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode     ClaudePermissionMode
		approval codex.ApprovalPolicy
		sandbox  protocol.SandboxPolicyKind
	}{
		{ClaudePermBypass, codex.ApprovalPolicyNever, protocol.SandboxPolicyKindDangerFullAccess},
		{ClaudePermPlan, codex.ApprovalPolicyOnRequest, protocol.SandboxPolicyKindReadOnly},
		{ClaudePermDefault, codex.ApprovalPolicyOnRequest, protocol.SandboxPolicyKindWorkspaceWrite},
		{ClaudePermAcceptEdits, codex.ApprovalPolicyOnRequest, protocol.SandboxPolicyKindWorkspaceWrite},
		{"", codex.ApprovalPolicyOnRequest, protocol.SandboxPolicyKindWorkspaceWrite},
	}
	for _, tc := range cases {
		approval, sandbox := codexTurnPolicy(tc.mode)
		if approval != tc.approval || sandbox != tc.sandbox {
			t.Errorf("codexTurnPolicy(%q) = (%s, %s), want (%s, %s)",
				tc.mode, approval, sandbox, tc.approval, tc.sandbox)
		}
	}
}

func TestCodexSandboxPolicyJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind protocol.SandboxPolicyKind
		want string
	}{
		{protocol.SandboxPolicyKindWorkspaceWrite, `{"type":"workspaceWrite"}`},
		{protocol.SandboxPolicyKindReadOnly, `{"type":"readOnly"}`},
		{protocol.SandboxPolicyKindDangerFullAccess, `{"type":"dangerFullAccess"}`},
	}
	for _, tc := range cases {
		if got := string(codexSandboxPolicyJSON(tc.kind)); got != tc.want {
			t.Errorf("codexSandboxPolicyJSON(%s) = %s, want %s", tc.kind, got, tc.want)
		}
	}
}

// Every turn must carry explicit approval AND sandbox policy: per-turn policy
// is the single mechanism (no thread-level defaults exist to fall back on),
// so a params build that omits either would silently defer to host codex
// config — an uncontrolled posture.
func TestCodexTurnParamsCarryExplicitPolicy(t *testing.T) {
	t.Parallel()
	for _, mode := range append(ClaudePermissionModes, "") {
		params, err := buildCodexTurnParams("thread-1", mode, "", SendMessageOpts{Text: "hi"})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if len(params.ApprovalPolicy) == 0 {
			t.Errorf("mode %q: approvalPolicy missing from turn params", mode)
		}
		if len(params.SandboxPolicy) == 0 {
			t.Errorf("mode %q: sandboxPolicy missing from turn params", mode)
		}
	}

	params, err := buildCodexTurnParams("thread-1", ClaudePermBypass, "", SendMessageOpts{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(params.ApprovalPolicy); got != `"never"` {
		t.Errorf("bypass approvalPolicy = %s, want %q", got, `"never"`)
	}
	if got := string(params.SandboxPolicy); got != `{"type":"dangerFullAccess"}` {
		t.Errorf("bypass sandboxPolicy = %s", got)
	}
}

func TestCodexTurnParamsInputValidation(t *testing.T) {
	t.Parallel()
	if _, err := buildCodexTurnParams("t", ClaudePermBypass, "", SendMessageOpts{}); err == nil {
		t.Error("empty prompt accepted, want error")
	}
	if _, err := buildCodexTurnParams("t", ClaudePermBypass, "", SendMessageOpts{
		Attachments: []Attachment{{Source: "data:image/png;base64,xxxx", Mime: "image/png"}},
	}); err == nil {
		t.Error("data: attachment accepted, want unsupported-scheme error")
	}
	params, err := buildCodexTurnParams("t", ClaudePermBypass, "gpt-5.5", SendMessageOpts{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if params.Model == nil || *params.Model != "gpt-5.5" {
		t.Errorf("model override not carried: %+v", params.Model)
	}
}
