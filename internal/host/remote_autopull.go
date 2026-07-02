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
	// Skip the whole pass when GitHub isn't connected — every pull would
	// just fail with ErrGitHubNotConnected.
	if creds, err := s.github.Store().Read(); err != nil || creds.AccessToken == "" {
		return
	}
	ids, err := materializedWorktreeIDs()
	if err != nil {
		s.log.Printf("cold-start auto-pull: list worktrees: %v", err)
		return
	}
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
			s.log.Printf("cold-start auto-pull: fast-forwarded %s", id)
		}
	}
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
