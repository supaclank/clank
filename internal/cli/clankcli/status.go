package clankcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/pkg/sessionsync"
)

// statusCmd registers `clank status` — a git-style summary of the
// current repo's worktree and its ownership on the active remote.
// Hits the active remote directly via NewRemoteClient (same path as
// `clank push --migrate`), so it works even when the local clankd
// isn't running.
func statusCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "status [repo-path]",
		Short: "Show the current worktree's local/remote status",
		Long: `Print a concise summary of this repo's worktree and where its
ownership currently lives. Without arguments, uses the current directory.

With -v, list what's out of sync: the unpushed commits, the uncommitted
files, and the changed opencode session titles.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			repoPath := ""
			if len(args) == 1 {
				repoPath = args[0]
			}
			if repoPath == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				repoPath = cwd
			}
			abs, err := filepath.Abs(repoPath)
			if err != nil {
				return fmt.Errorf("resolve repo path: %w", err)
			}
			out, err := runStatus(cmd.Context(), abs, verbose)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "list the unpushed commits, uncommitted files, and changed session titles")
	return cmd
}

// driftState classifies HOW a worktree is out of sync, so status can
// suggest the right verb (push/pull) instead of a generic "out of sync".
type driftState int

const (
	driftUnknown     driftState = iota // can't tell direction locally (zero value)
	driftAhead                         // local has commits the remote lacks → push
	driftBehind                        // remote has commits the local lacks → pull
	driftDiverged                      // both moved independently → push or pull
	driftUncommitted                   // commits match; only uncommitted changes differ → push
)

// statusReport is the data view rendered by `clank status`. Kept as a
// flat struct so tests can build it directly without I/O.
type statusReport struct {
	WorktreeID         string                     // empty when no worktree-id is cached for this clone
	WorktreeDir        string                     // basename of the worktree root; shown in the headline
	ActiveRemote       string                     // remote profile name; "" when none configured
	ActiveRemoteURL    string                     // for display
	SignedIn           bool                       // true when the active remote has an access token
	RemoteError        error                      // populated when the remote call failed (or refresh surfaced ErrUnauthorized)
	WorktreeFromRemote *daemonclient.WorktreeInfo // nil when the remote has no row for this worktree
	HasCheckpoint      bool                       // true when WorktreeFromRemote carries checkpoint metadata
	InSync             bool                       // true when local content SHAs match the remote's latest checkpoint
	WorkingTreeDirty   bool                       // true when the local tree has uncommitted changes (meaningful when InSync: dirty-but-synced)
	Drift              driftState                 // direction of drift when !InSync; driftUnknown if undeterminable
	DriftAhead         int                        // commits local is ahead by (0 when unknown or uncommitted-only)
	DriftBehind        int                        // commits local is behind by (0 when unknown)
	SessionsKnown      bool                       // true when the opencode session axis is determinable (opencode present, ≥1 session)
	UnsyncedSessions   int                        // opencode sessions changed since this machine last pushed (when SessionsKnown)

	// Verbose (-v) lists the changed opencode session titles under the
	// sessions bullet — the detail unique to clank. Commits and files are
	// already inspectable with git itself, so -v doesn't duplicate them.
	Verbose               bool     // -v: list changed session titles
	UnsyncedSessionLabels []string // changed-session labels (quoted title, or id when untitled)
}

// driftInfo is classifyDrift's result: a direction plus, when the heads
// differ and both are local objects, the commit counts each way.
type driftInfo struct {
	state  driftState
	ahead  int
	behind int
}

// runStatus assembles the report by reading the cached worktree id and
// querying the active remote (if configured). Rendering lives in
// renderStatusReport so tests can hit it without a running remote.
func runStatus(ctx context.Context, repoPath string, verbose bool) (string, error) {
	wtID, err := agent.ReadLocalWorktreeID(repoPath)
	if err != nil {
		return "", fmt.Errorf("read cached worktree id: %w", err)
	}

	rep := statusReport{WorktreeID: wtID, Verbose: verbose}

	// Worktree directory basename — what the user actually recognises.
	// Surface RepoRoot errors directly rather than falling back to the
	// raw repoPath; a status command silently mislabelling a non-repo
	// invocation would mask a real bug.
	var root string
	if wtID != "" {
		r, err := git.RepoRoot(repoPath)
		if err != nil {
			return "", fmt.Errorf("resolve worktree root: %w", err)
		}
		root = r
		rep.WorktreeDir = filepath.Base(root)
	}

	prefs, err := config.LoadPreferences()
	if err != nil {
		return "", fmt.Errorf("load preferences: %w", err)
	}
	if p := prefs.ActiveRemote(); p != nil {
		rep.ActiveRemote = prefs.Remote.Active
		rep.ActiveRemoteURL = p.GatewayURL
		rep.SignedIn = p.AccessToken != ""
	}

	// Session sync is a purely local axis (a session is only edited on the
	// machine it runs on), independent of the remote — compute it before the
	// remote block so it survives a remote-unreachable error. Gated on a
	// signed-in tracked worktree, the only case that reaches the detailed
	// status view that renders it.
	if wtID != "" && rep.SignedIn {
		unsynced, known := unsyncedSessions(ctx, root)
		rep.SessionsKnown = known
		rep.UnsyncedSessions = len(unsynced)
		if verbose {
			rep.UnsyncedSessionLabels = sessionLabels(unsynced)
		}
	}

	if rep.ActiveRemote != "" && wtID != "" {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		// Best-effort refresh before the actual call. A dead refresh
		// token surfaces as ErrUnauthorized and routes the renderer
		// straight to the "Not signed in" arm; transient errors fall
		// through to let the subsequent GetWorktree produce its own
		// (clearer) failure.
		if err := daemonclient.EnsureFreshActiveRemote(ctx); err != nil {
			if errors.Is(err, cloud.ErrUnauthorized) {
				rep.RemoteError = err
				return renderStatusReport(rep), nil
			}
		}

		cli, err := daemonclient.NewRemoteClient()
		if err != nil {
			rep.RemoteError = err
			return renderStatusReport(rep), nil
		}
		// GetWorktree (single) returns LatestCheckpointMetadata; the
		// list endpoint doesn't, so we can't compute parity from it.
		wt, err := cli.GetWorktree(ctx, wtID)
		if err != nil {
			if errors.Is(err, daemonclient.ErrWorktreeNotFound) {
				// Worktree row missing on remote — fall through with
				// WorktreeFromRemote nil; the renderer's "Not synced"
				// branch handles it.
				return renderStatusReport(rep), nil
			}
			rep.RemoteError = err
			return renderStatusReport(rep), nil
		}
		rep.WorktreeFromRemote = wt

		// Compute parity against the local working tree. A snapshot
		// failure (e.g. fresh repo with no commits) leaves InSync
		// false and HasCheckpoint reflecting the remote alone —
		// renderer treats that as "not yet pushed".
		if wt.LatestCheckpointMetadata != nil {
			rep.HasCheckpoint = true
			if snap, snapErr := snapshotRepo(ctx, repoPath, false); snapErr == nil {
				m := wt.LatestCheckpointMetadata
				rep.InSync = m.HeadCommit == snap.HeadCommit &&
					m.HeadRef == snap.HeadRef &&
					m.IndexTree == snap.IndexTree &&
					m.WorktreeTree == snap.WorktreeTree
				// Local dirtiness is its own axis (uncommitted changes vs
				// local HEAD), independent of the commit drift — surfaced as
				// the "Dirty state" line in either direction.
				if clean, cErr := git.IsClean(repoPath); cErr == nil {
					rep.WorkingTreeDirty = !clean
				}
				if !rep.InSync {
					d := classifyDrift(repoPath, snap.HeadCommit, m.HeadCommit)
					rep.Drift, rep.DriftAhead, rep.DriftBehind = d.state, d.ahead, d.behind
				}
			}
		}
	}

	return renderStatusReport(rep), nil
}

// classifyDrift determines the direction (and, where possible, the commit
// counts) of an out-of-sync worktree using only local git data (no backend
// round-trip). When the heads match, the commits are in sync and only
// uncommitted/index/worktree state differs (driftUncommitted). When the
// heads differ, the ahead/behind commit counts decide the direction; a
// remote head that isn't even a local object means the remote advanced
// past us (pull).
func classifyDrift(repoPath, localHead, remoteHead string) driftInfo {
	if localHead == "" || remoteHead == "" {
		return driftInfo{state: driftUnknown}
	}
	if localHead == remoteHead {
		// Same commit — committed history is in sync; the delta is purely
		// local working-tree / index changes.
		return driftInfo{state: driftUncommitted}
	}
	ahead, behind, err := git.AheadBehind(repoPath, localHead, remoteHead)
	if err != nil {
		// remoteHead isn't a local object → the remote moved to a commit we
		// don't have. Pull (fast-forward-guarded, so a true divergence is
		// refused safely) rather than guess a count.
		return driftInfo{state: driftBehind}
	}
	switch {
	case ahead > 0 && behind > 0:
		return driftInfo{state: driftDiverged, ahead: ahead, behind: behind}
	case behind > 0:
		return driftInfo{state: driftBehind, behind: behind}
	default:
		return driftInfo{state: driftAhead, ahead: ahead}
	}
}

// commits renders a commit count with correct pluralization.
func commits(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}

// sessions renders a session count with correct pluralization.
func sessions(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// countUnsyncedSessions reports how many of the worktree's current opencode
// sessions have changed since this machine last pushed them, and whether
// the axis is determinable at all (false → no opencode, no sessions, or any
// failure; status then omits the sessions line). Local-only and bounded:
// one ListSessions subprocess vs the on-disk last-pushed record. Claude is
// excluded — its sessions are never pushed (export unimplemented).
func unsyncedSessions(ctx context.Context, projectDir string) (unsynced []sessionsync.DiscoveredSession, known bool) {
	if _, err := exec.LookPath("opencode"); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	current, err := sessionsync.OpenCodeBackend{}.ListSessions(ctx, projectDir)
	if err != nil || len(current) == 0 {
		return nil, false
	}
	// A missing/corrupt record degrades to an empty one → everything counts
	// as unsynced (truthful; self-heals on the next push).
	rec, _ := agent.ReadSyncedSessions(projectDir)
	return unsyncedAgainst(current, rec), true
}

// unsyncedAgainst returns the current sessions that are absent from the
// last-pushed record or whose UpdatedAt has advanced past it. Both sides
// read UpdatedAt from the same backend source (opencode's `Updated`), so an
// unchanged session compares equal — use strict After, never time.Now().
func unsyncedAgainst(current []sessionsync.DiscoveredSession, rec agent.SyncedSessionRecord) []sessionsync.DiscoveredSession {
	var out []sessionsync.DiscoveredSession
	for _, s := range current {
		prev, ok := rec.Sessions[s.ExternalID]
		if !ok || s.UpdatedAt.After(prev.UpdatedAt) {
			out = append(out, s)
		}
	}
	return out
}

// sessionLabels renders changed-session detail lines for `status -v`: the
// quoted title, or the external id when a session has no title.
func sessionLabels(ss []sessionsync.DiscoveredSession) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s.Title != "" {
			out = append(out, fmt.Sprintf("%q", s.Title))
		} else {
			out = append(out, "session "+s.ExternalID)
		}
	}
	return out
}

// okLine / warnLine render a status bullet as a coloured glyph + neutral
// text. Only the glyph carries colour (green ✓ / yellow •), so the block
// reads calmly instead of as competing fully-coloured phrases.
func okLine(text string) string {
	return "  " + styleOK.Render("✓") + " " + text + "\n"
}

func warnLine(text string) string {
	return "  " + styleWarn.Render("•") + " " + text + "\n"
}

// syncCTA renders the bottom call-to-action for an out-of-sync worktree.
// The framing is dimmed so the command — the one neutral-bright token —
// is what the eye lands on, without spending another accent colour.
func syncCTA(remote string, push, pull bool) string {
	dim := styleDim.Render
	switch {
	case push && pull:
		return dim("Run ") + "`clank push`" + dim(" or ") + "`clank pull`" + dim(" to reconcile with "+remote+" remote.")
	case pull:
		return dim("Run ") + "`clank pull`" + dim(" to sync with "+remote+" remote.")
	default:
		return dim("Run ") + "`clank push`" + dim(" to sync with "+remote+" remote.")
	}
}

// (Styles moved to style.go so push/pull/status share the same palette.)

// renderStatusReport produces a git-status-flavoured paragraph:
// one-line headline + optional bullet under it. Branches by scenario
// rather than always-show-every-field.
func renderStatusReport(rep statusReport) string {
	var sb strings.Builder

	// Layered hints — earliest applicable action wins so we never tell
	// the user to push/pull when they actually need to login first.

	// No remote configured at all.
	if rep.ActiveRemote == "" {
		if rep.WorktreeID == "" {
			sb.WriteString(styleWarn.Render("Not synced") + "\n")
			sb.WriteString("  " + styleDim.Render("Run `clank remote add`, then `clank push` to register.") + "\n")
			return sb.String()
		}
		sb.WriteString("On worktree " + styleWorktree.Render(rep.WorktreeDir) + "\n")
		sb.WriteString("  " + styleLocalOwner.Render("Owned by this laptop") + " " + styleDim.Render("(no remote configured)") + "\n")
		return sb.String()
	}

	// Remote configured but the user's session is missing or rejected.
	// Always wins over any "push/pull" suggestion — those calls would
	// 401 anyway.
	if !rep.SignedIn || errors.Is(rep.RemoteError, cloud.ErrUnauthorized) {
		sb.WriteString(styleWarn.Render("Not signed in") + " to " + styleRemoteOwner.Render(rep.ActiveRemote) + " — run " + styleCmdHint.Render("`clank login`") + "\n")
		return sb.String()
	}

	// Worktree not on the remote (either no local id, or the remote
	// successfully responded but doesn't know this worktree). User just
	// needs to push; surface as "Not synced" with no headline noise.
	if rep.WorktreeID == "" || (rep.RemoteError == nil && rep.WorktreeFromRemote == nil) {
		sb.WriteString(styleWarn.Render("Not synced") + "\n")
		sb.WriteString("  " + styleDim.Render(fmt.Sprintf("Run `clank push` to sync to the %s remote.", rep.ActiveRemote)) + "\n")
		return sb.String()
	}

	// Headline always names the worktree by directory basename — the
	// ULID is implementation detail the user doesn't care to track.
	sb.WriteString("On worktree ")
	sb.WriteString(styleWorktree.Render(rep.WorktreeDir))
	sb.WriteString("\n")

	// ctaBlock prints a blank separator line then the action, so the state
	// bullets and the "what to run" guidance don't read as one dense block.
	ctaBlock := func(push, pull bool) {
		sb.WriteString("\n  " + syncCTA(rep.ActiveRemote, push, pull) + "\n")
	}

	// detail lists the -v diff under a bullet (indented + dimmed); a no-op
	// without -v or when there's nothing to list.
	detail := func(lines []string) {
		if !rep.Verbose {
			return
		}
		for _, ln := range lines {
			sb.WriteString("      " + styleDim.Render(ln) + "\n")
		}
	}

	// dirtyBullet adds the uncommitted-changes line when the local tree is
	// dirty — the dirty-state axis is independent of commit drift, so a
	// worktree can be e.g. ahead AND have unsynced uncommitted changes.
	dirtyBullet := func() {
		if rep.WorkingTreeDirty {
			sb.WriteString(warnLine("Dirty state not synced"))
		}
	}

	// sessionBullet adds the opencode session line when that axis is known —
	// also independent of code drift (sessions can lag while code is synced).
	sessionBullet := func() {
		if !rep.SessionsKnown {
			return
		}
		if rep.UnsyncedSessions == 0 {
			sb.WriteString(okLine("Sessions in sync"))
		} else {
			sb.WriteString(warnLine(sessions(rep.UnsyncedSessions) + " not synced"))
			detail(rep.UnsyncedSessionLabels)
		}
	}

	switch {
	case rep.RemoteError != nil:
		sb.WriteString("  Sync state " + styleErr.Render("unknown") + " — " + styleRemoteOwner.Render(rep.ActiveRemote) + " remote unreachable: " + styleDim.Render(rep.RemoteError.Error()) + "\n")
	case !rep.HasCheckpoint:
		sb.WriteString("  " + styleDim.Render("Not yet pushed to "+rep.ActiveRemote+" remote") + "\n")
	case rep.InSync:
		// Code is in sync. When there's a session axis to show, decompose into
		// Commits/Dirty/Sessions bullets so an unsynced session isn't masked
		// by a single "In sync" summary; otherwise keep the compact summary.
		switch {
		case rep.SessionsKnown:
			sb.WriteString(okLine("Commits in sync"))
			if rep.WorkingTreeDirty {
				sb.WriteString(okLine("Dirty state in sync"))
			}
			sessionBullet()
			if rep.UnsyncedSessions > 0 {
				ctaBlock(true, false)
			}
		case rep.WorkingTreeDirty:
			sb.WriteString(okLine("Commits in sync"))
			sb.WriteString(okLine("Dirty state in sync"))
		default:
			sb.WriteString(okLine("In sync with " + rep.ActiveRemote + " remote"))
		}
	case rep.Drift == driftUncommitted:
		sb.WriteString(okLine("Commits in sync"))
		sb.WriteString(warnLine("Dirty state not synced"))
		sessionBullet()
		ctaBlock(true, false)
	case rep.Drift == driftAhead:
		sb.WriteString(warnLine("Ahead by " + commits(rep.DriftAhead)))
		dirtyBullet()
		sessionBullet()
		ctaBlock(true, false)
	case rep.Drift == driftBehind:
		sb.WriteString(warnLine("Behind by " + commits(rep.DriftBehind)))
		dirtyBullet()
		sessionBullet()
		ctaBlock(false, true)
	case rep.Drift == driftDiverged:
		sb.WriteString(warnLine(fmt.Sprintf("Diverged — %d ahead, %d behind", rep.DriftAhead, rep.DriftBehind)))
		dirtyBullet()
		sessionBullet()
		ctaBlock(true, true)
	default:
		sb.WriteString(warnLine("Out of sync"))
		dirtyBullet()
		sessionBullet()
		ctaBlock(true, true)
	}

	return sb.String()
}
