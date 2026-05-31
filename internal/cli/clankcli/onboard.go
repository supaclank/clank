package clankcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/clanksync/triggers"
	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/git"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// isInteractive reports whether clank may prompt the user: both stdin and
// stdout must be terminals. Autopush triggers (the Claude Code Stop hook,
// the opencode plugin) run non-interactively, so this gates EVERY prompt
// — a prompt that blocked on stdin there would hang the agent's turn.
//
// When in/out are not *os.File (e.g. a test buffer, or a pipe), this is
// false, which keeps the safe non-interactive error path.
func isInteractive(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return false
	}
	out, ok := cmd.OutOrStdout().(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// readLine reads a single line from the command's input WITHOUT buffering
// past the newline, so consecutive prompts sharing one reader (e.g. piped
// test input "y\n2\n") don't lose bytes to a discarded bufio buffer. The
// trailing newline and any CR are stripped.
func readLine(cmd *cobra.Command) (string, error) {
	in := cmd.InOrStdin()
	if in == nil {
		in = os.Stdin
	}
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("read input: %w", err)
		}
	}
	return strings.TrimRight(sb.String(), "\r"), nil
}

// allHarnesses is the safe default when the user hasn't chosen: install
// triggers for every supported harness.
func allHarnesses() []string {
	return []string{triggers.HarnessClaudeCode, triggers.HarnessOpenCode}
}

// pickHarnesses asks which coding-agent harnesses to auto-sync sessions
// for. A bare Enter (or an unrecognized answer) selects both. The Claude
// option is the Claude Code CLI / Agent SDK Stop hook — it covers any app
// built on the Claude CLI/SDK, but NOT Claude Desktop (untrackable).
func pickHarnesses(cmd *cobra.Command) ([]string, error) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Which coding agents should clank auto-sync sessions for?")
	fmt.Fprintln(out, "  1) Claude Code (CLI / Agent SDK)")
	fmt.Fprintln(out, "  2) opencode")
	fmt.Fprintln(out, "  3) Both")
	fmt.Fprint(out, "Select [3]: ")
	line, err := readLine(cmd)
	if err != nil {
		return nil, err
	}
	switch strings.TrimSpace(line) {
	case "1":
		return []string{triggers.HarnessClaudeCode}, nil
	case "2":
		return []string{triggers.HarnessOpenCode}, nil
	default:
		return allHarnesses(), nil
	}
}

// ensureHarnessTriggers resolves which harnesses to install autopush
// triggers for and installs them. Resolution order:
//   - already chosen (prefs.SyncHarnesses) → install those (idempotent).
//   - interactive + unchosen → prompt, persist the choice, install.
//   - non-interactive + unchosen → install both (safe default), no persist.
func ensureHarnessTriggers(cmd *cobra.Command, interactive bool) error {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return fmt.Errorf("load preferences: %w", err)
	}
	if len(prefs.SyncHarnesses) > 0 {
		return installTriggersFor(cmd, prefs.SyncHarnesses)
	}
	if !interactive {
		return installTriggersFor(cmd, allHarnesses())
	}
	chosen, err := pickHarnesses(cmd)
	if err != nil {
		return err
	}
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.SyncHarnesses = chosen
	}); err != nil {
		return fmt.Errorf("save harness selection: %w", err)
	}
	return installTriggersFor(cmd, chosen)
}

// remoteCreds fills any unset baseURL/token from the active remote.
func remoteCreds(baseURL, token string) (string, string, error) {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return "", "", fmt.Errorf("load preferences: %w", err)
	}
	if p := prefs.ActiveRemote(); p != nil {
		if baseURL == "" {
			baseURL = p.GatewayURL
		}
		if token == "" {
			token = p.AccessToken
		}
	}
	return baseURL, token, nil
}

// ensureLoggedIn returns a sync client for the active remote, signing the
// user in first when needed. An explicit token (self-hosted static
// bearer, CI) bypasses the refresh + login flow. On a TTY a signed-out or
// session-expired user is offered an interactive sign-in; otherwise the
// caller gets today's "not signed in"/"session expired" error so autopush
// hooks never block.
func ensureLoggedIn(ctx context.Context, cmd *cobra.Command, baseURL, token string) (*syncclient.Client, error) {
	// Refresh BEFORE first use so the client below carries a fresh bearer.
	// Skip when --token is explicit: refreshing the profile's expired
	// refresh_token would abort a push that has independent valid creds.
	if token == "" {
		if err := daemonclient.EnsureFreshActiveRemote(ctx); err != nil {
			if !errors.Is(err, cloud.ErrUnauthorized) {
				return nil, fmt.Errorf("refresh remote session: %w", err)
			}
			if !isInteractive(cmd) {
				return nil, fmt.Errorf("session expired — run `clank login` to sign in again")
			}
			if err := runLogin(ctx, cmd, "", ""); err != nil {
				return nil, err
			}
		}
	}

	baseURL, token, err := remoteCreds(baseURL, token)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		return nil, fmt.Errorf("--base-url is required (or set CLANK_GATEWAY_URL, or configure an active remote via `clank remote add`)")
	}
	if token == "" {
		// Signed out: offer sign-in on a TTY, else keep the clean error.
		if isInteractive(cmd) {
			if err := runLogin(ctx, cmd, "", ""); err != nil {
				return nil, err
			}
			if baseURL, token, err = remoteCreds(baseURL, token); err != nil {
				return nil, err
			}
		}
		if token == "" {
			return nil, fmt.Errorf("not signed in — run `clank login` to sign in to the active remote")
		}
	}
	return syncclient.New(syncclient.Config{BaseURL: baseURL, AuthToken: token})
}

// currentDirForHint returns the cwd for a best-effort tracking nudge, or
// "" if it can't be resolved (the nudge is then skipped).
func currentDirForHint() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// hintRepoTracking prints a one-line nudge when dir is a git repo that
// isn't tracked yet — so a returning user knows how to start syncing it
// without re-running onboarding. Best-effort and silent when there's
// nothing useful to say (not a repo, already tracked, or auto-push on).
func hintRepoTracking(cmd *cobra.Command, prefs config.Preferences, dir string) error {
	if prefs.AutoPushAllRepos || dir == "" {
		return nil // every repo auto-tracks on push; no nudge needed.
	}
	root, err := git.RepoRoot(dir)
	if err != nil {
		return nil // not a git repo (or no git) → nothing to say.
	}
	id, err := agent.ReadLocalWorktreeID(root)
	if err != nil || id != "" {
		return nil // already tracked, or can't tell → no nudge.
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nThis repo (%s) isn't tracked yet — run `clank push` here to start syncing it.\n", filepath.Base(root))
	return nil
}

// ensureTracked resolves the worktree-id for absRepo, registering it if
// needed. A cached id is returned as-is. Untracked: with AutoPushAllRepos
// it auto-registers; on a TTY (interactive) it offers to track and runs
// first-time harness onboarding; otherwise it returns today's "run clank
// init" error. displayName "" defaults to the repo's basename.
func ensureTracked(ctx context.Context, cmd *cobra.Command, cli *syncclient.Client, absRepo, displayName string, interactive bool) (string, error) {
	id, err := agent.ReadLocalWorktreeID(absRepo)
	if err != nil {
		return "", fmt.Errorf("load cached worktree id: %w", err)
	}
	if id != "" {
		return id, nil
	}

	prefs, err := config.LoadPreferences()
	if err != nil {
		return "", fmt.Errorf("load preferences: %w", err)
	}
	if !prefs.AutoPushAllRepos {
		if !interactive {
			return "", fmt.Errorf("this worktree isn't tracked — run `clank init` (or `clank init --global` to auto-track every repo)")
		}
		track, err := confirmYesNo(cmd, fmt.Sprintf("Track %s for clank push? [Y/n] ", filepath.Base(absRepo)), true)
		if err != nil {
			return "", err
		}
		if !track {
			return "", fmt.Errorf("not tracked — run `clank init` when you want to start syncing this repo")
		}
	}

	name := displayName
	if name == "" {
		name = filepath.Base(absRepo)
	}
	id, err = cli.RegisterWorktree(ctx, name)
	if err != nil {
		return "", fmt.Errorf("register worktree: %w", err)
	}
	if err := agent.WriteLocalWorktreeID(absRepo, id); err != nil {
		return "", fmt.Errorf("cache worktree id: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "registered worktree %s as %q\n", id, name)

	// First interactive track also runs harness onboarding — idempotent,
	// a no-op once a harness is chosen. Skipped non-interactively (a hook
	// must not install triggers from within a triggered push).
	if interactive {
		if err := ensureHarnessTriggers(cmd, true); err != nil {
			return "", err
		}
	}
	return id, nil
}
