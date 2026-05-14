package clankcli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// pushSessionLeg is the laptop-side orchestration of the session
// export inside a `clank push --migrate`. Runs AFTER PushCheckpoint
// (code blobs in S3) and BEFORE MigrateWorktree (ownership flip):
//
//  1. Ask the local clank-host (via clankd's Unix-socket proxy) to
//     quiesce + export every session in the worktree → returns a
//     build_id + manifest entries.
//  2. Ask the gateway's sync server for presigned PUT URLs covering
//     each session blob + the session-manifest.json sidecar.
//  3. Hand those URLs back to clank-host so it can PUT the blobs
//     directly to S3 (bytes never traverse this process).
//
// On failure the migration aborts before ownership transfers, so the
// caller retains a fully-local worktree they can retry against.
//
// timer is non-nil; pass a disabled timer when not measuring.
func pushSessionLeg(cmd *cobra.Command, timer *phaseTimer, worktreeID, checkpointID string, gateway *syncclient.Client) error {
	// 1. Build (quiesce + export) via the local daemon → clank-host proxy.
	hostCli, err := daemonclient.NewLocalClient()
	if err != nil {
		return fmt.Errorf("local daemon client: %w", err)
	}

	// Generous deadline because exports of large sessions are slow.
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	done := timer.Start("build session export")
	build, err := hostCli.BuildSessionCheckpoint(buildCtx, worktreeID, checkpointID)
	done()
	if err != nil {
		return err
	}

	// Surface skipped and busy sessions so the user knows what
	// happened. Claude sessions are silently dropped per plan §G,
	// but we tell the user; busy sessions were aborted by the
	// host's quiesce step.
	for _, sk := range build.Skipped {
		fmt.Fprintf(cmd.OutOrStdout(), "  skip session %s (backend=%s): %s\n", sk.SessionID, sk.Backend, sk.Reason)
	}
	for _, e := range build.Entries {
		if e.WasBusy {
			fmt.Fprintf(cmd.OutOrStdout(), "  interrupted busy session %s (will resume idle on the remote)\n", e.SessionID)
		}
	}

	if len(build.Entries) == 0 && len(build.Skipped) == 0 {
		// No opencode sessions in this worktree — nothing to do.
		// (We still upload an empty session-manifest.json so the
		// remote can distinguish "v1+ checkpoint, no sessions" from
		// "pre-feature checkpoint, manifest absent".)
	}

	sessionIDs := make([]string, len(build.Entries))
	for i, e := range build.Entries {
		sessionIDs[i] = e.SessionID
	}

	// 2. Mint presigned PUT URLs from the gateway's sync server.
	presignCtx, cancelPresign := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancelPresign()
	done = timer.Start("request session upload URLs")
	urls, err := gateway.RequestSessionUploadURLs(presignCtx, checkpointID, sessionIDs)
	done()
	if err != nil {
		return err
	}

	// 3. Tell clank-host to upload the blobs to those URLs.
	uploadCtx, cancelUpload := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelUpload()
	done = timer.Start("upload sessions")
	err = hostCli.UploadSessionCheckpoint(uploadCtx, build.BuildID, checkpointID, urls.SessionPutURLs, urls.SessionManifestPutURL)
	done()
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "pushed %d session(s) for checkpoint %s\n", len(build.Entries), checkpointID)
	return nil
}
