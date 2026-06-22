package storage_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
			if !errors.Is(err, storage.ErrInvalidPathComponent) {
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
	// The trailing slash is mandatory — see the boundary regression below.
	if want := "user-A/"; got != want {
		t.Fatalf("KeyForUserPrefix mismatch: got %q want %q", got, want)
	}
}

func TestKeyForUserPrefix_RejectsPathEscape(t *testing.T) {
	t.Parallel()
	for _, userID := range []string{"", "..", "u/v", "u\\v", ".hidden", strings.Repeat("x", 129)} {
		t.Run(userID, func(t *testing.T) {
			t.Parallel()
			if _, err := storage.KeyForUserPrefix(userID); !errors.Is(err, storage.ErrInvalidPathComponent) {
				t.Fatalf("userID %q: expected ErrInvalidPathComponent, got %v", userID, err)
			}
		})
	}
}

// TestMemory_DeletePrefix is the headline tenant-erasure test: deleting
// "user1/" purges only user1's blobs. The "user10/" survivor is the
// boundary regression — without the trailing slash in the prefix,
// "user1" would also match "user10/", deleting another tenant's data.
func TestMemory_DeletePrefix(t *testing.T) {
	t.Parallel()
	mem := storage.NewMemory()
	defer mem.Close()

	mem.Put("user1/checkpoints/wt/ck/manifest.json", []byte("a"))
	mem.Put("user1/heads/deadbeef.bundle", []byte("b"))
	mem.Put("user10/checkpoints/wt/ck/manifest.json", []byte("c"))
	mem.Put("user2/checkpoints/wt/ck/manifest.json", []byte("d"))

	prefix, err := storage.KeyForUserPrefix("user1")
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.DeletePrefix(context.Background(), prefix); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}

	got := mem.Keys()
	want := map[string]bool{
		"user10/checkpoints/wt/ck/manifest.json": true,
		"user2/checkpoints/wt/ck/manifest.json":  true,
	}
	if len(got) != len(want) {
		t.Fatalf("after DeletePrefix keys=%v, want %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("unexpected surviving key %q (deleted too much?)", k)
		}
	}
}

func TestMemory_DeletePrefix_Idempotent(t *testing.T) {
	t.Parallel()
	mem := storage.NewMemory()
	defer mem.Close()
	// Deleting an absent prefix is a no-op, not an error.
	if err := mem.DeletePrefix(context.Background(), "ghost/"); err != nil {
		t.Fatalf("DeletePrefix on empty prefix: %v", err)
	}
}

func TestMemory_RoundTrip(t *testing.T) {
	t.Parallel()
	mem := storage.NewMemory()
	defer mem.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key, err := storage.KeyFor("u", "wt", "ck", storage.BlobUncommitted)
	if err != nil {
		t.Fatal(err)
	}

	putURL, err := mem.PresignPut(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte("git bundle contents")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT got %d", resp.StatusCode)
	}

	exists, err := mem.Exists(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected exists=true after PUT")
	}

	getURL, err := mem.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET got %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("body mismatch: got %q want %q", got, body)
	}
}

func TestMemory_PresignPutRejectsGetMethod(t *testing.T) {
	t.Parallel()
	mem := storage.NewMemory()
	defer mem.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key, err := storage.KeyFor("u", "wt", "ck", storage.BlobManifest)
	if err != nil {
		t.Fatal(err)
	}
	putURL, err := mem.PresignPut(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// A PUT-presigned URL must not accept GET — the op param guards it.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, putURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for GET on a PUT URL, got %d", resp.StatusCode)
	}
}

func TestMemory_ExpiredURLRefused(t *testing.T) {
	t.Parallel()
	mem := storage.NewMemory()
	defer mem.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key, err := storage.KeyFor("u", "wt", "ck", storage.BlobUncommitted)
	if err != nil {
		t.Fatal(err)
	}
	// Negative TTL — already expired before the request lands.
	putURL, err := mem.PresignPut(ctx, key, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, putURL, strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for expired URL, got %d", resp.StatusCode)
	}
}

func TestMemory_ExistsFalseForMissing(t *testing.T) {
	t.Parallel()
	mem := storage.NewMemory()
	defer mem.Close()
	ctx := context.Background()
	exists, err := mem.Exists(ctx, "u/checkpoints/wt/ck/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected exists=false for missing key")
	}
}
