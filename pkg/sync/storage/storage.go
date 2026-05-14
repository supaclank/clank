// Package storage is the object-storage layer for clank-sync's
// checkpoint substrate. Bundles never traverse the sync server in
// memory; the laptop and sprite upload/download via presigned URLs
// minted here.
//
// The Storage interface is provider-agnostic — the S3 implementation
// works against AWS S3, Cloudflare R2, Tigris, MinIO, and any other
// S3-compatible API. The Memory implementation is for tests.
//
// Path convention (see KeyFor): every blob lives at
//
//	checkpoints/<userID>/<worktreeID>/<checkpointID>/<blob>
//
// where userID always comes from validated token claims, never from
// untrusted request input. KeyFor refuses any component containing
// path-escape sequences ("..", "/", "\\") so a bug at a higher layer
// can't smuggle one tenant's path-prefix into another.
package storage

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Blob enumerates the well-known per-checkpoint blob names. Adding a
// new kind requires a code change here so handlers can't smuggle a
// new path component via untrusted request input.
type Blob string

const (
	BlobHeadCommit  Blob = "headCommit.bundle"
	BlobIncremental Blob = "incremental.bundle"
	BlobManifest    Blob = "manifest.json"
)

// validBlobs is the closed set of acceptable blob names. Any value
// outside this set returns ErrInvalidPathComponent from KeyFor.
var validBlobs = map[Blob]bool{
	BlobHeadCommit:      true,
	BlobIncremental:     true,
	BlobManifest:        true,
	BlobSessionManifest: true,
}

// ErrInvalidPathComponent is returned by KeyFor when one of the
// inputs would produce an unsafe storage key. Surfaced as a typed
// error so handlers can return 400 instead of 500.
var ErrInvalidPathComponent = errors.New("storage: invalid path component")

// ErrNotFound is returned by Get/Exists when no object exists at the
// requested key. Wrapped, not unwrapped — callers should errors.Is.
var ErrNotFound = errors.New("storage: object not found")

// Storage is the minimal contract clank-sync needs from object storage.
// Implementations MUST be safe for concurrent use.
type Storage interface {
	// PresignPut returns a presigned PUT URL valid for ttl. The URL
	// is itself the capability — anyone holding it can upload to that
	// key until ttl expires. Callers MUST scope key construction via
	// KeyFor, never accept raw paths from untrusted input.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (url string, err error)

	// PresignGet returns a presigned GET URL valid for ttl. Same
	// capability semantics as PresignPut.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (url string, err error)

	// Exists reports whether an object exists at key. Used for
	// content-addressed dedup of headCommit bundles — if the SHA-keyed
	// object is already there, we skip the PUT URL entirely.
	Exists(ctx context.Context, key string) (bool, error)
}

// KeyFor builds the storage key for a (userID, worktreeID, checkpointID, blob)
// quad. This is the SINGLE function that maps tenant-scoped identifiers
// to a storage path. Every component is validated for path safety; userID
// in particular MUST come from authenticated token claims, never from
// query parameters or request body.
//
// For the headCommit blob specifically, the checkpointID should encode
// the head SHA so that re-pushes of the same HEAD reuse the existing
// object (content-addressed dedup). For incremental and manifest
// blobs, the checkpointID is the per-push ULID.
func KeyFor(userID, worktreeID, checkpointID string, blob Blob) (string, error) {
	if !validBlobs[blob] {
		return "", fmt.Errorf("%w: blob %q not in validBlobs", ErrInvalidPathComponent, blob)
	}
	for _, c := range []struct {
		name, value string
	}{
		{"userID", userID},
		{"worktreeID", worktreeID},
		{"checkpointID", checkpointID},
	} {
		if err := validateComponent(c.name, c.value); err != nil {
			return "", err
		}
	}
	return path.Join("checkpoints", userID, worktreeID, checkpointID, string(blob)), nil
}

// validateComponent rejects empty strings, anything containing path
// separators or escape sequences, and anything starting with a dot
// (would shadow ".gitignore"-style hidden entries).
func validateComponent(name, v string) error {
	if v == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidPathComponent, name)
	}
	if strings.ContainsAny(v, "/\\") || strings.Contains(v, "..") {
		return fmt.Errorf("%w: %s contains path separator or .. (%q)", ErrInvalidPathComponent, name, v)
	}
	if strings.HasPrefix(v, ".") {
		return fmt.Errorf("%w: %s starts with dot (%q)", ErrInvalidPathComponent, name, v)
	}
	if len(v) > 128 {
		return fmt.Errorf("%w: %s exceeds 128 chars", ErrInvalidPathComponent, name)
	}
	return nil
}
