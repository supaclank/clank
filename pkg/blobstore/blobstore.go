// Package blobstore is a provider-agnostic object-storage layer: a
// minimal presigned-URL contract plus path-safety primitives, with no
// knowledge of what the blobs are. Code bundles, session exports, and
// user image uploads each sit on top of it via their own key builders.
//
// The S3 implementation works against AWS S3, Cloudflare R2, Tigris,
// MinIO, and any other S3-compatible API. The Memory implementation is
// for tests.
//
// Path safety: ValidateComponent / ValidateContentHash reject any path
// component that could escape its tenant prefix ("..", "/", "\\", a
// leading dot, or an over-long value). Key builders in dependent
// packages MUST route every untrusted component through them; userID in
// particular MUST come from authenticated token claims, never request
// input.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidPathComponent is returned by ValidateComponent /
// ValidateContentHash when an input would produce an unsafe storage
// key. Surfaced as a typed error so handlers can return 400 not 500.
var ErrInvalidPathComponent = errors.New("blobstore: invalid path component")

// ErrNotFound is returned when no object exists at the requested key.
// Wrapped, not unwrapped — callers should errors.Is.
var ErrNotFound = errors.New("blobstore: object not found")

// Storage is the minimal contract for object storage. Implementations
// MUST be safe for concurrent use.
type Storage interface {
	// PresignPut returns a presigned PUT URL valid for ttl. The URL is
	// itself the capability — anyone holding it can upload to that key
	// until ttl expires. Callers MUST scope key construction via a
	// validated key builder, never accept raw paths from untrusted input.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (url string, err error)

	// PresignGet returns a presigned GET URL valid for ttl. Same
	// capability semantics as PresignPut.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (url string, err error)

	// Exists reports whether an object exists at key. Used for
	// content-addressed dedup — if the keyed object is already there, the
	// caller can skip the PUT URL entirely.
	Exists(ctx context.Context, key string) (bool, error)

	// DeletePrefix removes every object whose key starts with prefix.
	// Idempotent — deleting an empty/already-gone prefix is not an error.
	// Used for tenant erasure (account deletion): one sweep of "<userID>/"
	// purges all of a user's blobs. Callers MUST build the prefix via a
	// validated key builder so an empty/escaped value can't widen the
	// sweep to the whole bucket.
	DeletePrefix(ctx context.Context, prefix string) error
}

// ValidateComponent rejects empty strings, anything containing path
// separators or escape sequences, anything starting with a dot (would
// shadow ".gitignore"-style hidden entries), and anything over 128
// chars. name is only used to render the error.
func ValidateComponent(name, v string) error {
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

// ValidateContentHash requires exactly 64 lowercase-hex chars (a sha256
// digest) so a caller can't smuggle an arbitrary path component through
// a content-hash slot.
func ValidateContentHash(h string) error {
	if len(h) != 64 {
		return fmt.Errorf("%w: contentHash must be 64 hex chars, got %d", ErrInvalidPathComponent, len(h))
	}
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("%w: contentHash has non-hex char %q", ErrInvalidPathComponent, string(c))
		}
	}
	return nil
}
