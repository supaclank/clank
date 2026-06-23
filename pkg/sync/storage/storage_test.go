package storage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/sync/storage"
)

func TestKeyFor_Valid(t *testing.T) {
	t.Parallel()
	got, err := storage.KeyFor("user-A", "wt-123", "ck-456", storage.BlobManifest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "user-A/checkpoints/wt-123/ck-456/manifest.json"
	if got != want {
		t.Fatalf("KeyFor mismatch: got %q want %q", got, want)
	}
}

func TestKeyForHead(t *testing.T) {
	t.Parallel()
	got, err := storage.KeyForHead("user-A", "deadbeefcafe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "user-A/heads/deadbeefcafe.bundle"; got != want {
		t.Fatalf("KeyForHead mismatch: got %q want %q", got, want)
	}
	if _, err := storage.KeyForHead("", "sha"); err == nil {
		t.Error("empty userID should error")
	}
	if _, err := storage.KeyForHead("u", "../escape"); err == nil {
		t.Error("path-escape headSHA should error")
	}
}

func TestKeyFor_RejectsPathEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                             string
		userID, worktreeID, checkpointID string
		blob                             storage.Blob
	}{
		{"userID with ..", "..", "wt", "ck", storage.BlobManifest},
		{"userID with /", "u/v", "wt", "ck", storage.BlobManifest},
		{"userID with \\", "u\\v", "wt", "ck", storage.BlobManifest},
		{"userID empty", "", "wt", "ck", storage.BlobManifest},
		{"userID dot prefix", ".hidden", "wt", "ck", storage.BlobManifest},
		{"worktreeID with ..", "u", "..", "ck", storage.BlobManifest},
		{"worktreeID with /", "u", "wt/x", "ck", storage.BlobManifest},
		{"checkpointID with ..", "u", "wt", "..", storage.BlobManifest},
		{"checkpointID with /", "u", "wt", "ck/x", storage.BlobManifest},
		{"unknown blob", "u", "wt", "ck", storage.Blob("evil.sh")},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := storage.KeyFor(c.userID, c.worktreeID, c.checkpointID, c.blob)
			if !errors.Is(err, blobstore.ErrInvalidPathComponent) {
				t.Fatalf("expected ErrInvalidPathComponent, got %v", err)
			}
		})
	}
}

func TestKeyFor_NoCrossTenantAncestry(t *testing.T) {
	t.Parallel()
	// For two distinct userIDs, KeyFor must produce paths where neither
	// is a prefix of the other. This is the catastrophic-leak guard
	// from the plan: a bug in the caller cannot smuggle one tenant's
	// path-prefix into another.
	keyA, err := storage.KeyFor("user-A", "wt", "ck", storage.BlobManifest)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := storage.KeyFor("user-B", "wt", "ck", storage.BlobManifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(keyA, keyB) || strings.HasPrefix(keyB, keyA) {
		t.Fatalf("cross-tenant key prefix overlap: %q vs %q", keyA, keyB)
	}
}

func TestKeyForUserPrefix_Valid(t *testing.T) {
	t.Parallel()
	got, err := storage.KeyForUserPrefix("user-A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The trailing slash is mandatory — see the boundary regression in
	// pkg/blobstore's DeletePrefix tests.
	if want := "user-A/"; got != want {
		t.Fatalf("KeyForUserPrefix mismatch: got %q want %q", got, want)
	}
}

func TestKeyForUserPrefix_RejectsPathEscape(t *testing.T) {
	t.Parallel()
	for _, userID := range []string{"", "..", "u/v", "u\\v", ".hidden", strings.Repeat("x", 129)} {
		t.Run(userID, func(t *testing.T) {
			t.Parallel()
			if _, err := storage.KeyForUserPrefix(userID); !errors.Is(err, blobstore.ErrInvalidPathComponent) {
				t.Fatalf("userID %q: expected ErrInvalidPathComponent, got %v", userID, err)
			}
		})
	}
}
