package clankcli

import (
	"context"
	"fmt"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sessionsync"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// phaseSyncingSessions labels the post-checkpoint session-sync phase on the
// push status line.
const phaseSyncingSessions = "Syncing sessions"

// pushSessions pushes the worktree's opencode sessions, daemon-free. Runs
// AFTER PushCheckpoint (code blobs in S3), attached to its checkpointID:
//
//  1. Discover + export the sessions filed under projectDir directly from
//     opencode's storage (no clank-host).
//  2. Mint presigned PUT URLs from the gateway and upload each blob + the
//     session-manifest.json sidecar straight to object storage.
//
// It returns the exported count and any skipped sessions so the caller can
// print a summary AFTER tearing down the progress line — printing inline
// would corrupt the rewriting status line. timer is non-nil.
func pushSessions(ctx context.Context, timer *phaseTimer, projectDir, checkpointID string, gateway *syncclient.Client) (exported int, skipped []sessionsync.SkippedSession, err error) {
	done := timer.Start("export sessions")
	export, err := sessionsync.ExportWorktreeSessions(ctx, projectDir)
	done()
	if err != nil {
		return 0, nil, fmt.Errorf("export sessions: %w", err)
	}
	defer export.Cleanup()

	done = timer.Start("upload sessions")
	err = sessionsync.UploadSessions(ctx, gateway, checkpointID, export.Exported)
	done()
	if err != nil {
		return 0, nil, fmt.Errorf("upload sessions: %w", err)
	}

	// Record what we just pushed so `clank status` can detect later local
	// session changes. Best-effort: the push already succeeded, so a sidecar
	// write failure must not fail it — status just won't reflect this push.
	_ = agent.WriteSyncedSessions(projectDir, syncedSessionsRecord(export.Exported))

	return len(export.Exported), export.Skipped, nil
}

// syncedSessionsRecord converts exported sessions into the local
// last-pushed record, keyed by backend-native ExternalID and stamped with
// the same UpdatedAt that went into the uploaded SessionManifest.
func syncedSessionsRecord(exported []sessionsync.ExportedSession) agent.SyncedSessionRecord {
	rec := agent.SyncedSessionRecord{Sessions: make(map[string]agent.SyncedSession, len(exported))}
	for _, e := range exported {
		rec.Sessions[e.Entry.ExternalID] = agent.SyncedSession{
			Backend:     e.Entry.Backend,
			UpdatedAt:   e.Entry.UpdatedAt,
			Fingerprint: e.Fingerprint,
		}
	}
	return rec
}
