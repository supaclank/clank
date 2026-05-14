package storage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/sync/storage"
)

func TestKeyForSession_Valid(t *testing.T) {
	t.Parallel()
	got, err := storage.KeyForSession("user-A", "wt-123", "ck-456", "01HX0001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "checkpoints/user-A/wt-123/ck-456/sessions/01HX0001.json"
	if got != want {
		t.Fatalf("KeyForSession mismatch: got %q want %q", got, want)
	}
}

func TestKeyForSession_RejectsPathEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                                            string
		userID, worktreeID, checkpointID, sessionULID   string
	}{
		{"sessionULID with ..", "u", "wt", "ck", ".."},
		{"sessionULID with /", "u", "wt", "ck", "ses/x"},
		{"sessionULID with \\", "u", "wt", "ck", "ses\\x"},
		{"sessionULID empty", "u", "wt", "ck", ""},
		{"sessionULID dot prefix", "u", "wt", "ck", ".hidden"},
		{"userID with ..", "..", "wt", "ck", "s1"},
		{"worktreeID with /", "u", "wt/x", "ck", "s1"},
		{"checkpointID empty", "u", "wt", "", "s1"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := storage.KeyForSession(c.userID, c.worktreeID, c.checkpointID, c.sessionULID)
			if !errors.Is(err, storage.ErrInvalidPathComponent) {
				t.Fatalf("expected ErrInvalidPathComponent, got %v", err)
			}
		})
	}
}

// Session keys must sit UNDER the same checkpoint prefix as the code
// bundles so a single ownership-flip covers both legs.
func TestKeyForSession_SharesCheckpointPrefix(t *testing.T) {
	t.Parallel()
	codeKey, err := storage.KeyFor("u", "wt", "ck", storage.BlobManifest)
	if err != nil {
		t.Fatal(err)
	}
	sessKey, err := storage.KeyForSession("u", "wt", "ck", "s1")
	if err != nil {
		t.Fatal(err)
	}
	codePrefix := strings.TrimSuffix(codeKey, "/manifest.json")
	if !strings.HasPrefix(sessKey, codePrefix+"/") {
		t.Fatalf("session key %q does not share checkpoint prefix with %q", sessKey, codeKey)
	}
}

func TestKeyForSession_NoCrossTenantAncestry(t *testing.T) {
	t.Parallel()
	keyA, err := storage.KeyForSession("user-A", "wt", "ck", "s1")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := storage.KeyForSession("user-B", "wt", "ck", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(keyA, keyB) || strings.HasPrefix(keyB, keyA) {
		t.Fatalf("cross-tenant session key prefix overlap: %q vs %q", keyA, keyB)
	}
}

// BlobSessionManifest is a sidecar at the same checkpoint level as
// BlobManifest. Confirms it's accepted by KeyFor.
func TestKeyFor_SessionManifestBlobAccepted(t *testing.T) {
	t.Parallel()
	got, err := storage.KeyFor("u", "wt", "ck", storage.BlobSessionManifest)
	if err != nil {
		t.Fatalf("BlobSessionManifest should be valid: %v", err)
	}
	want := "checkpoints/u/wt/ck/session-manifest.json"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
