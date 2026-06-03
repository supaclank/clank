package storage

import "path"

// BlobSessionManifest is the per-checkpoint sidecar listing all session
// blobs that ride alongside the code bundles. It sits at the same
// checkpoint prefix as headCommit.bundle and manifest.json.
const BlobSessionManifest Blob = "session-manifest.json"

// keySessionDir is the directory component for content-addressed session
// export blobs under a worktree. Not a Blob constant because it's a
// directory, not a leaf name (see KeyForSessionBlob).
const keySessionDir = "sessions"

// KeyForSessionBlob builds the storage key for one session's export
// blob, content-addressed under the WORKTREE (not a checkpoint):
//
//	<userID>/worktrees/<worktreeID>/sessions/<externalID>/<contentHash>
//
// A session's exported bytes are identical across the checkpoints that
// carry it, so it's stored once and each checkpoint's manifest references
// it — the same content-addressed, cross-checkpoint sharing KeyForHead
// gives code bundles. externalID is the backend-native (cross-machine)
// id; contentHash is the sha256 of the blob. Every component is
// validated; userID MUST come from authenticated token claims.
func KeyForSessionBlob(userID, worktreeID, externalID, contentHash string) (string, error) {
	for _, c := range []struct {
		name, value string
	}{
		{"userID", userID},
		{"worktreeID", worktreeID},
		{"externalID", externalID},
	} {
		if err := validateComponent(c.name, c.value); err != nil {
			return "", err
		}
	}
	if err := validateContentHash(contentHash); err != nil {
		return "", err
	}
	return path.Join(userID, "worktrees", worktreeID, keySessionDir, externalID, contentHash), nil
}
