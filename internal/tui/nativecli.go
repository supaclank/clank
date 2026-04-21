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

// nativeCLICmd builds the exec.Cmd for opening a session in its native backend
// CLI. Currently only supports OpenCode sessions.
//
// For OpenCode: runs `opencode attach <serverURL> --session <externalID>`.
// `opencode attach`'s --dir flag is optional — when omitted it derives the
// project from the server URL — so we don't pass it. This keeps nativecli
// path-free, in line with §7.1's "TUI uses ExternalID+Backend for native-CLI
// shell-out, derives display name from GitRef".
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
		return exec.Command("opencode", "attach", info.ServerURL,
			"--session", info.ExternalID,
		), nil
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
