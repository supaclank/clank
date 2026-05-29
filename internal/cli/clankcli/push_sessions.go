package clankcli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/pkg/sessionsync"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// pushSessionLeg pushes the worktree's opencode sessions, daemon-free.
// Runs AFTER PushCheckpoint (code blobs in S3), attached to its
// checkpointID:
//
//  1. Discover + export the sessions filed under projectDir directly
//     from opencode's storage (no clank-host).
//  2. Mint presigned PUT URLs from the gateway and upload each blob +
//     the session-manifest.json sidecar straight to object storage.
//
// timer is non-nil; pass a disabled timer when not measuring.
func pushSessionLeg(cmd *cobra.Command, timer *phaseTimer, projectDir, checkpointID string, gateway *syncclient.Client) error {
	// Generous deadline: exporting large sessions is slow.
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer cancel()

	done := timer.Start("export sessions")
	export, err := sessionsync.ExportWorktreeSessions(ctx, projectDir)
	done()
	if err != nil {
		return fmt.Errorf("export sessions: %w", err)
	}
	defer export.Cleanup()

	for _, sk := range export.Skipped {
		fmt.Fprintf(cmd.OutOrStdout(), "  skip session %s (backend=%s): %s\n", sk.ExternalID, sk.Backend, sk.Reason)
	}

	done = timer.Start("upload sessions")
	err = sessionsync.UploadSessions(ctx, gateway, checkpointID, export.Exported)
	done()
	if err != nil {
		return fmt.Errorf("upload sessions: %w", err)
	}

	if len(export.Exported) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "pushed %d session(s) for checkpoint %s\n", len(export.Exported), checkpointID)
	}
	return nil
}
