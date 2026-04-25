package tui

import (
	"fmt"
	"os/exec"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

// nativeCLIReturnMsg is sent when the user returns from a native CLI session.
type nativeCLIReturnMsg struct {
	sessionID string
	err       error
}

// CLI binary names and flags. Centralized as constants per AGENTS.md
// "no magic strings": easier to grep when upgrading CLI versions.
const (
	opencodeBin         = "opencode"
	opencodeAttachCmd   = "attach"
	opencodeSessionFlag = "--session"
	claudeBin           = "claude"
	claudeResumeFlag    = "--resume"
)

// nativeCLICmd builds the exec.Cmd for opening a session in its native backend
// CLI.
//
// For OpenCode: runs `opencode attach <serverURL> --session <externalID>`.
// `opencode attach`'s --dir flag is optional — when omitted it derives the
// project from the server URL — so we don't pass it. This keeps the OpenCode
// branch path-free, in line with §7.1's "TUI uses ExternalID+Backend for
// native-CLI shell-out, derives display name from GitRef".
//
// For Claude Code: runs `claude --resume <externalID>` with cmd.Dir set to
// the repo's LocalPath. Unlike OpenCode there is no server URL to anchor the
// project, so cmd.Dir is required. The claude CLI's `--resume` resolves
// sessions across the repo's git worktrees automatically (mirroring the SDK's
// ListSessions behaviour — see internal/host/backends.go:194-196), so passing
// the repo root is sufficient even for sessions started in a worktree.
func nativeCLICmd(info *agent.SessionInfo) (*exec.Cmd, error) {
	if info == nil {
		return nil, fmt.Errorf("no session info")
	}
	switch info.Backend {
	case agent.BackendOpenCode:
		if info.ExternalID == "" {
			return nil, fmt.Errorf("session has no external ID (still starting?)")
		}
		if info.ServerURL == "" {
			return nil, fmt.Errorf("no OpenCode server URL for session %q (daemon may still be starting the server)", info.ID)
		}
		return exec.Command(opencodeBin, opencodeAttachCmd, info.ServerURL,
			opencodeSessionFlag, info.ExternalID,
		), nil
	case agent.BackendClaudeCode:
		if info.ExternalID == "" {
			return nil, fmt.Errorf("session has no external ID (still starting?)")
		}
		// Claude has no server URL; cmd.Dir anchors `claude --resume` to the
		// right project. Per AGENTS.md "no fallbacks", refuse to inherit the
		// TUI's cwd silently — that would resume the session against an
		// unrelated tree if the user happened to launch clank elsewhere.
		if info.GitRef.LocalPath == "" {
			return nil, fmt.Errorf("session %q has no local path; cannot launch claude CLI", info.ID)
		}
		cmd := exec.Command(claudeBin, claudeResumeFlag, info.ExternalID)
		cmd.Dir = info.GitRef.LocalPath
		return cmd, nil
	default:
		return nil, fmt.Errorf("native CLI not supported for %s backend", info.Backend)
	}
}

// openNativeCLI returns a tea.Cmd that suspends the TUI, launches the native
// CLI for the given session, and sends a nativeCLIReturnMsg when done.
func openNativeCLI(info *agent.SessionInfo) tea.Cmd {
	cmd, err := nativeCLICmd(info)
	if err != nil {
		sessionID := ""
		if info != nil {
			sessionID = info.ID
		}
		return func() tea.Msg {
			return nativeCLIReturnMsg{sessionID: sessionID, err: err}
		}
	}
	sessionID := info.ID
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return nativeCLIReturnMsg{sessionID: sessionID, err: err}
	})
}
