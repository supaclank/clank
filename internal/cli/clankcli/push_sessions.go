package clankcli

import (
	"context"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sessionsync"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// Session-sync phase labels on the push status line. The leg is split into
// export (copy + hash changed transcripts locally) and upload (ship the
// blobs + manifest) so the live (i/N) covers the whole span — on a
// fast/local gateway the upload is near instant, so without the export
// label the counter would only flash.
const (
	phaseExportingSessions = "Exporting sessions"
	phaseUploadingSessions = "Uploading sessions"
)

// pushSessions pushes the worktree's changed sessions, daemon-free. Runs
// AFTER PushCheckpoint (code blobs in S3), attached to its checkpointID:
//
//  1. PreparePush discovers sessions + exports ONLY those changed since the
//     last push, content-hashing each, and assembles the COMPLETE manifest
//     (changed blobs plus unchanged ones referenced by their stored hash).
//  2. Mint presigned PUT URLs and upload the changed blobs + the complete
//     session-manifest.json sidecar straight to object storage.
//  3. Persist the rebuilt last-pushed record so `clank status` and the next
//     push agree on what's now synced.
//
// Returns the uploaded (changed) count and any skipped sessions so the
// caller can print a summary AFTER tearing down the progress line. timer is
// non-nil.
func pushSessions(ctx context.Context, timer *phaseTimer, projectDir, checkpointID string, gateway *syncclient.Client, obs syncclient.PushObserver) (exported int, skipped []sessionsync.SkippedSession, err error) {
	rec, _ := agent.ReadSyncedSessions(projectDir)

	if obs != nil {
		obs.Phase(phaseExportingSessions)
	}
	done := timer.Start("export sessions")
	plan, err := sessionsync.PreparePush(ctx, projectDir, rec, obs)
	done()
	if err != nil {
		return 0, nil, fmt.Errorf("export sessions: %w", err)
	}
	defer plan.Cleanup()

	if obs != nil {
		obs.Phase(phaseUploadingSessions)
	}
	done = timer.Start("upload sessions")
	err = sessionsync.UploadSessions(ctx, gateway, checkpointID, plan, obs)
	done()
	if err != nil {
		return 0, nil, fmt.Errorf("upload sessions: %w", err)
	}

	// Record what's now synced (+ when) so `clank status` can detect later
	// local session changes and show push recency. Best-effort: the push
	// already succeeded, so a sidecar write failure must not fail it.
	plan.Record.LastPushedAt = time.Now()
	_ = agent.WriteSyncedSessions(projectDir, plan.Record)

	return len(plan.Upload), plan.Skipped, nil
}
