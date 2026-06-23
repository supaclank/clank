// Package storage builds tenant-scoped storage keys for clank-sync's
// checkpoint substrate on top of the provider-agnostic pkg/blobstore.
//
// Path convention (see KeyFor): every blob lives under a per-tenant
// prefix at
//
//	<userID>/checkpoints/<worktreeID>/<checkpointID>/<blob>
//
// where userID always comes from validated token claims, never from
// untrusted request input. Components are validated via
// blobstore.ValidateComponent so a bug at a higher layer can't smuggle
// one tenant's path-prefix into another.
package storage

import (
	"fmt"
	"path"

	"github.com/acksell/clank/pkg/blobstore"
)

// Blob enumerates the well-known per-checkpoint blob names. Adding a
// new kind requires a code change here so handlers can't smuggle a new
// path component via untrusted request input.
type Blob string

const (
	BlobUncommitted Blob = "uncommitted.bundle"
	BlobManifest    Blob = "manifest.json"
)

// validBlobs is the closed set of acceptable per-checkpoint blob names.
// The head bundle is NOT here — it's content-addressed via KeyForHead,
// shared across checkpoints. Any value outside this set returns
// blobstore.ErrInvalidPathComponent from KeyFor.
var validBlobs = map[Blob]bool{
	BlobUncommitted:     true,
	BlobManifest:        true,
	BlobSessionManifest: true,
}

// KeyFor builds the storage key for a (userID, worktreeID, checkpointID,
// blob) quad. This is the SINGLE function that maps tenant-scoped
// identifiers to a storage path. Every component is validated for path
// safety; userID in particular MUST come from authenticated token
// claims, never from query parameters or request body.
//
// The per-checkpoint blobs (uncommitted bundle, manifest, session
// blobs) use the per-push checkpoint ULID. The head bundle is NOT a
// per-checkpoint blob — it's content-addressed by HEAD SHA via
// KeyForHead and shared across checkpoints.
func KeyFor(userID, worktreeID, checkpointID string, blob Blob) (string, error) {
	if !validBlobs[blob] {
		return "", fmt.Errorf("%w: blob %q not in validBlobs", blobstore.ErrInvalidPathComponent, blob)
	}
	for _, c := range []struct {
		name, value string
	}{
		{"userID", userID},
		{"worktreeID", worktreeID},
		{"checkpointID", checkpointID},
	} {
		if err := blobstore.ValidateComponent(c.name, c.value); err != nil {
			return "", err
		}
	}
	return path.Join(userID, "checkpoints", worktreeID, checkpointID, string(blob)), nil
}

// KeyForHead builds the storage key for a head bundle, content-addressed
// by the HEAD commit SHA and shared across a user's checkpoints/worktrees
// (a commit is a commit — the same SHA reaches the same objects). Lives
// under the same per-tenant prefix as KeyFor. userID MUST come from
// authenticated token claims.
//
//	<userID>/heads/<headSHA>.bundle
func KeyForHead(userID, headSHA string) (string, error) {
	for _, c := range []struct {
		name, value string
	}{
		{"userID", userID},
		{"headSHA", headSHA},
	} {
		if err := blobstore.ValidateComponent(c.name, c.value); err != nil {
			return "", err
		}
	}
	return path.Join(userID, "heads", headSHA+".bundle"), nil
}

// KeyForUserPrefix builds the storage key prefix covering every blob a
// user owns (checkpoints, session blobs, head bundles all live under
// "<userID>/"). The trailing slash is load-bearing: without it the
// prefix "user1" would also match "user10/", deleting another tenant's
// data. userID MUST come from authenticated token claims; it is run
// through blobstore.ValidateComponent, so an empty or path-escaping
// userID returns ErrInvalidPathComponent rather than a prefix that could
// sweep the whole bucket.
func KeyForUserPrefix(userID string) (string, error) {
	if err := blobstore.ValidateComponent("userID", userID); err != nil {
		return "", err
	}
	return userID + "/", nil
}
