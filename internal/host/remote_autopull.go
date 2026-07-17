package host

// Cold-start auto-pull. When a suspended sprite wakes (or the daemon
// otherwise starts), fast-forward each materialized, GitHub-backed,
// clean, idle worktree from its remote — so work pushed while the sprite
// slept is there when the user reconnects. Pull, not push: publishing
// stays a manual action (per product decision). This is the ONLY
// automatic remote-sync trigger; everything else is user-initiated.

import (
	"context"
	"errors"
	"os"
	"time"
)

// startColdStartAutoPull kicks a one-shot best-effort fast-forward pass in
// a goroutine so it never blocks Init. Skips entirely when GitHub has no
// manager.
func (s *Service) startColdStartAutoPull(ctx context.Context) {
	if s.github == nil {
		return
	}
	go s.coldStartAutoPull(ctx)
}

func (s *Service) coldStartAutoPull(ctx context.Context) {
	// Skip the whole pass when no GitHub credential is available (store
	// or gh CLI fallback) — every pull would just fail with
	// ErrGitHubNotConnected.
	if _, err := s.github.AccessToken(); err != nil {
		return
	}
	ids, err := materializedWorktreeIDs()
	if err != nil {
		s.log.Printf("cold-start auto-pull: list worktrees: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	// Timed because the pass runs git fetches during the cold-boot
	// window, competing for the same disk/CPU as the first session open.
	passStart := time.Now()
	var pulled int
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// Don't fast-forward under a live agent — it could change files
		// mid-session. (At a true cold start nothing is running yet, but a
		// user can reconnect and start a session while this pass runs.)
		if busy, err := s.WorktreeHasActiveSession(ctx, id); err == nil && busy {
			continue
		}
		// TODO(ai-review): give this a per-worktree timeout so one hung
		// remote can't stall the whole pass — needs context plumbed through
		// git.Fetch (exec.Command -> exec.CommandContext), not just a ctx
		// wrap here (remoteContextFor/runPull don't honor ctx today).
		// https://github.com/Acksell/clank/pull/158#discussion_r3596541125
		res, err := s.PullFromRemote(ctx, id)
		if err != nil {
			// Expected skips (dirty, diverged, no upstream, no origin,
			// detached HEAD) are silent — only unexpected errors are logged.
			if !errors.Is(err, ErrNoOriginRemote) && !errors.Is(err, ErrNoUpstream) &&
				!errors.Is(err, ErrWorktreeDirty) && !errors.Is(err, ErrRemoteDiverged) &&
				!errors.Is(err, ErrDetachedHead) {
				s.log.Printf("cold-start auto-pull: skip %s: %v", id, err)
			}
			continue
		}
		if res.FastForwarded {
			pulled++
			s.log.Printf("cold-start auto-pull: fast-forwarded %s", id)
		}
	}
	s.log.Printf("cold-start auto-pull: done in %s (worktrees=%d fast-forwarded=%d)",
		time.Since(passStart).Round(time.Millisecond), len(ids), pulled)
}

// materializedWorktreeIDs lists the worktree IDs present under
// workRootDir() ($HOME/work/<id>). Each subdirectory name is a worktree ID
// resolvable by workDirFor. Non-ULID entries — chiefly the repos/ subtree
// holding the bare canonicals — are skipped: they aren't worktrees.
func materializedWorktreeIDs() ([]string, error) {
	root, err := workRootDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no work dir yet → nothing to pull
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && isULIDLike(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
