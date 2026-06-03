package storage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/sync/storage"
)

// BlobSessionManifest is a sidecar at the same checkpoint level as
// BlobManifest. Confirms it's accepted by KeyFor.
func TestKeyFor_SessionManifestBlobAccepted(t *testing.T) {
	t.Parallel()
	got, err := storage.KeyFor("u", "wt", "ck", storage.BlobSessionManifest)
	if err != nil {
		t.Fatalf("BlobSessionManifest should be valid: %v", err)
	}
	want := "u/checkpoints/wt/ck/session-manifest.json"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// sha256 of the empty string — a real 64-hex digest for key tests.
const validSessionHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestKeyForSessionBlob_Valid(t *testing.T) {
	t.Parallel()
	got, err := storage.KeyForSessionBlob("user-A", "wt-123", "ext-sess-1", validSessionHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "user-A/worktrees/wt-123/sessions/ext-sess-1/" + validSessionHash
	if got != want {
		t.Fatalf("KeyForSessionBlob mismatch: got %q want %q", got, want)
	}
}

func TestKeyForSessionBlob_RejectsBadHash(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, hash string }{
		{"too short", "abc"},
		{"too long", validSessionHash + "00"},
		{"non-hex", strings.Repeat("g", 64)},
		{"uppercase", strings.ToUpper(validSessionHash)},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := storage.KeyForSessionBlob("u", "wt", "ext", c.hash)
			if !errors.Is(err, storage.ErrInvalidPathComponent) {
				t.Fatalf("expected ErrInvalidPathComponent, got %v", err)
			}
		})
	}
}

func TestKeyForSessionBlob_RejectsPathEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                         string
		userID, worktreeID, external string
	}{
		{"externalID with ..", "u", "wt", ".."},
		{"externalID with /", "u", "wt", "ses/x"},
		{"externalID empty", "u", "wt", ""},
		{"externalID dot prefix", "u", "wt", ".hidden"},
		{"userID with ..", "..", "wt", "s1"},
		{"worktreeID with /", "u", "wt/x", "s1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := storage.KeyForSessionBlob(c.userID, c.worktreeID, c.external, validSessionHash)
			if !errors.Is(err, storage.ErrInvalidPathComponent) {
				t.Fatalf("expected ErrInvalidPathComponent, got %v", err)
			}
		})
	}
}

func TestKeyForSessionBlob_NoCrossTenantAncestry(t *testing.T) {
	t.Parallel()
	keyA, err := storage.KeyForSessionBlob("user-A", "wt", "ext", validSessionHash)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := storage.KeyForSessionBlob("user-B", "wt", "ext", validSessionHash)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(keyA, keyB) || strings.HasPrefix(keyB, keyA) {
		t.Fatalf("cross-tenant session blob key prefix overlap: %q vs %q", keyA, keyB)
	}
}
