package storage

import "path"

// BlobSessionManifest is the per-checkpoint sidecar listing all session
// blobs that ride alongside the code bundles. It sits at the same
// checkpoint prefix as headCommit.bundle and manifest.json.
const BlobSessionManifest Blob = "session-manifest.json"

// keySessionDir is the prefix for per-session export blobs under a
// checkpoint. Not a Blob constant because it's a directory, not a
// leaf name — accessing it requires a sessionULID component too
// (see KeyForSession).
const keySessionDir = "sessions"

// KeyForSession builds the storage key for one session's exported
// blob, addressed as a sibling of the code bundles inside a
// checkpoint. Layout:
//
//	checkpoints/<userID>/<worktreeID>/<checkpointID>/sessions/<sessionULID>.json
//
// sessionULID is the host-side ULID (SessionInfo.ID) — cross-machine
// stable. Every component is validated for path safety; userID MUST
// come from authenticated token claims.
func KeyForSession(userID, worktreeID, checkpointID, sessionULID string) (string, error) {
	for _, c := range []struct {
		name, value string
	}{
		{"userID", userID},
		{"worktreeID", worktreeID},
		{"checkpointID", checkpointID},
		{"sessionULID", sessionULID},
	} {
		if err := validateComponent(c.name, c.value); err != nil {
			return "", err
		}
	}
	return path.Join("checkpoints", userID, worktreeID, checkpointID, keySessionDir, sessionULID+".json"), nil
}

