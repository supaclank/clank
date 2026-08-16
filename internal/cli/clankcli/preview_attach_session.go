package clankcli

// `clank preview --attach`: bind the preview overlay to an existing
// agent session instead of letting the first prompt create a new one.
// Bare --attach opens an interactive picker; --attach=<id> resolves the
// clank session id or the backend's external session id directly.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/tui"
)

// previewAttachSelect is --attach's value when no session id is given
// (pflag NoOptDefVal): open the interactive session picker.
const previewAttachSelect = "select"

// errPreviewAttachAborted ends the run when the user cancels the
// session picker (esc/ctrl+c). Expected, not a failure: the run paths
// catch it and exit cleanly instead of surfacing an error.
var errPreviewAttachAborted = errors.New("preview canceled from the session picker")

// previewAttachDiscoverBackends is the rediscovery sweep for an id that
// isn't registered yet — the same pair the inbox's import action scans.
var previewAttachDiscoverBackends = []agent.BackendType{agent.BackendOpenCode, agent.BackendClaudeCode}

// resolveAttachSession maps the --attach flag onto a session. Returns
// nil when the flag is unset, or when the user skipped the picker (the
// preview then continues with a fresh session).
func resolveAttachSession(ctx context.Context, client *daemonclient.Client, attachFlag, projectDir string, in io.Reader, out io.Writer) (*agent.SessionInfo, error) {
	switch attachFlag {
	case "":
		return nil, nil
	case previewAttachSelect:
		return pickAttachSession(ctx, client, projectDir, in, out)
	default:
		return resolveAttachSessionByID(ctx, client, attachFlag, projectDir)
	}
}

// pickAttachSession hands the terminal to the session picker and maps
// its outcome: a choice attaches, canceling (esc/ctrl+c) abandons the
// preview run — --attach was explicit, so there is no silent
// fall-through to a fresh session.
func pickAttachSession(ctx context.Context, client *daemonclient.Client, projectDir string, in io.Reader, out io.Writer) (*agent.SessionInfo, error) {
	if !isInteractiveTerminal(in, out) {
		return nil, fmt.Errorf("--attach needs a terminal to pick a session; pass --attach=<session-id> instead")
	}

	tui.ApplyPreferredTheme()
	model := tui.NewSessionPickerModel(client, projectDir)

	cleanupLogs := redirectLogToFile()
	defer cleanupLogs()

	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	if _, err := program.Run(); err != nil {
		return nil, fmt.Errorf("run session picker: %w", err)
	}

	result := model.Result()
	if result.IsAborted || result.SessionID == "" {
		return nil, errPreviewAttachAborted
	}
	// The picker only offers ids from the live catalog, so this lookup
	// is a fetch of the full record, not a second search.
	return resolveAttachSessionByID(ctx, client, result.SessionID, projectDir)
}

// resolveAttachSessionByID finds the session whose clank id or
// backend-external id equals id. An unknown id triggers one rediscovery
// sweep of the project dir before giving up — the session may exist in
// the backend's archive without being registered with clank yet.
func resolveAttachSessionByID(ctx context.Context, client *daemonclient.Client, id, projectDir string) (*agent.SessionInfo, error) {
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	if s := sessionMatchingID(sessions, id); s != nil {
		return s, nil
	}

	var discoverErrs []string
	for _, bt := range previewAttachDiscoverBackends {
		if derr := client.Sessions().Discover(ctx, bt, projectDir); derr != nil {
			discoverErrs = append(discoverErrs, fmt.Sprintf("%s: %v", bt, derr))
		}
	}
	sessions, err = client.Sessions().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions after rediscovery: %w", err)
	}
	if s := sessionMatchingID(sessions, id); s != nil {
		return s, nil
	}

	msg := fmt.Sprintf("session %q not found, even after rediscovering %s — run clank preview --attach to pick from the list", id, projectDir)
	if len(discoverErrs) > 0 {
		msg += " (rediscovery errors: " + strings.Join(discoverErrs, "; ") + ")"
	}
	return nil, errors.New(msg)
}

// attachedSessionID unwraps an optional attach resolution into the
// overlay's session id ("" = no attach).
func attachedSessionID(session *agent.SessionInfo) string {
	if session == nil {
		return ""
	}
	return session.ID
}

// sessionMatchingID returns the session whose clank id or external id
// equals id, or nil.
func sessionMatchingID(sessions []agent.SessionInfo, id string) *agent.SessionInfo {
	for i := range sessions {
		if sessions[i].ID == id || (sessions[i].ExternalID != "" && sessions[i].ExternalID == id) {
			return &sessions[i]
		}
	}
	return nil
}

// attachedSessionBackend reconciles --backend with the attached
// session's backend: unset adopts the session's, a matching value is
// confirmed, a conflicting one fails fast.
func attachedSessionBackend(session *agent.SessionInfo, backendFlag string) (agent.BackendType, error) {
	if backendFlag == "" {
		return session.Backend, nil
	}
	parsed, err := agent.ParseBackend(backendFlag)
	if err != nil {
		return "", err
	}
	if parsed != session.Backend {
		return "", fmt.Errorf("--backend %s conflicts with the attached session's backend %s — drop the flag to use the session's backend", parsed, session.Backend)
	}
	return session.Backend, nil
}

var (
	previewULIDPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$`)
	previewUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
)

// routeAttachSessionArg supports `clank preview --attach <id>`. pflag
// binds an optional-value flag only with `=`, so the spaced form arrives
// as a bare --attach plus a positional — the same slot a launch name
// occupies. Session ids are the three shapes clank and its backends
// mint (opencode "ses_…", Claude's UUID, clank's ULID), none of which a
// launch name from .clank/launch.yaml looks like, so an id-shaped
// positional is claimed as the session and anything else stays a launch
// name (`clank preview web-app --attach` keeps opening the picker).
func routeAttachSessionArg(attachFlag string, args []string) (string, []string) {
	if attachFlag != previewAttachSelect || len(args) != 1 || !looksLikeSessionID(args[0]) {
		return attachFlag, args
	}
	return args[0], nil
}

// looksLikeSessionID reports whether arg has the shape of a session id:
// opencode's "ses_…", a UUID (Claude), or a 26-char ULID (clank).
func looksLikeSessionID(arg string) bool {
	if strings.HasPrefix(arg, "ses_") {
		return true
	}
	return previewULIDPattern.MatchString(arg) || previewUUIDPattern.MatchString(arg)
}
