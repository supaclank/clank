package sync

import (
	"context"
	"fmt"

	"github.com/acksell/clank/pkg/sync/storage"
)

// PurgeUser erases all of a user's sync substrate: object-store blobs
// (checkpoints, session blobs, head bundles), head-bundle rows, and every
// worktree with its checkpoint rows. The account-deletion endpoint calls
// this after the user's compute has been destroyed.
//
// Each step is idempotent, so a partially-failed purge is safe to retry; on
// the first error it aborts, leaving the remaining rows for the retry. Blobs
// are deleted first via a single "<userID>/" prefix sweep — that purge is
// keyed only on userID and independent of row state, so it stays correct even
// if a prior attempt already dropped some rows.
func (s *Server) PurgeUser(ctx context.Context, userID string) error {
	prefix, err := storage.KeyForUserPrefix(userID)
	if err != nil {
		return fmt.Errorf("purge user %s: %w", userID, err)
	}
	if err := s.cfg.Storage.DeletePrefix(ctx, prefix); err != nil {
		return fmt.Errorf("purge user %s blobs: %w", userID, err)
	}
	if err := s.cfg.Store.DeleteHeadBundlesByUser(ctx, userID); err != nil {
		return fmt.Errorf("purge user %s head bundles: %w", userID, err)
	}
	wts, err := s.cfg.Store.ListWorktreesByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("purge user %s: list worktrees: %w", userID, err)
	}
	// Mirror DeleteWorktree's order: checkpoints before the worktree row so a
	// mid-purge failure never leaves the row gone with orphan checkpoints.
	for _, wt := range wts {
		if err := s.cfg.Store.DeleteCheckpointsByWorktree(ctx, wt.ID); err != nil {
			return fmt.Errorf("purge user %s: delete checkpoints (worktree=%s): %w", userID, wt.ID, err)
		}
		if err := s.cfg.Store.DeleteWorktree(ctx, wt.ID); err != nil {
			return fmt.Errorf("purge user %s: delete worktree %s: %w", userID, wt.ID, err)
		}
	}
	return nil
}
