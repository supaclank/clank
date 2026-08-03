package blobstore_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/pkg/blobstore"
)

const testKey = "user-A/checkpoints/wt/ck/uncommitted.bundle"

func TestMemory_RoundTrip(t *testing.T) {
	t.Parallel()
	mem := blobstore.NewMemory()
	defer mem.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	putURL, err := mem.PresignPut(ctx, testKey, time.Minute)
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

	exists, err := mem.Exists(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected exists=true after PUT")
	}

	getURL, err := mem.PresignGet(ctx, testKey, time.Minute)
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
	mem := blobstore.NewMemory()
	defer mem.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	putURL, err := mem.PresignPut(ctx, testKey, time.Minute)
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
	mem := blobstore.NewMemory()
	defer mem.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Negative TTL — already expired before the request lands.
	putURL, err := mem.PresignPut(ctx, testKey, -time.Second)
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
	mem := blobstore.NewMemory()
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

// TestMemory_DeletePrefix is the headline tenant-erasure test: deleting
// "user1/" purges only user1's blobs. The "user10/" survivor is the boundary
// regression — without the trailing slash in the prefix, "user1" would also
// match "user10/", deleting another tenant's data.
func TestMemory_DeletePrefix(t *testing.T) {
	t.Parallel()
	mem := blobstore.NewMemory()
	defer mem.Close()

	mem.Put("user1/checkpoints/wt/ck/manifest.json", []byte("a"))
	mem.Put("user1/heads/deadbeef.bundle", []byte("b"))
	mem.Put("user10/checkpoints/wt/ck/manifest.json", []byte("c"))
	mem.Put("user2/checkpoints/wt/ck/manifest.json", []byte("d"))

	if err := mem.DeletePrefix(context.Background(), "user1/"); err != nil {
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
	mem := blobstore.NewMemory()
	defer mem.Close()
	// Deleting an absent prefix is a no-op, not an error.
	if err := mem.DeletePrefix(context.Background(), "ghost/"); err != nil {
		t.Fatalf("DeletePrefix on empty prefix: %v", err)
	}
}

// TestMemory_DeletePrefix_RejectsEmpty guards the catastrophic case: an empty
// prefix matches every key in the store and would wipe all tenants' data.
func TestMemory_DeletePrefix_RejectsEmpty(t *testing.T) {
	t.Parallel()
	mem := blobstore.NewMemory()
	defer mem.Close()
	mem.Put("user1/blob", []byte("data"))
	if err := mem.DeletePrefix(context.Background(), ""); err == nil {
		t.Fatal("DeletePrefix(\"\") should return an error, not wipe the entire store")
	}
	if keys := mem.Keys(); len(keys) != 1 {
		t.Fatalf("store was mutated by rejected DeletePrefix: keys=%v", keys)
	}
}
