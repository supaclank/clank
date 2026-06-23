package sync_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/blobstore"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// newPurgeServer is newDeleteServer but also returns the object store so a
// test can seed and assert on blobs (which PurgeUser sweeps by prefix).
func newPurgeServer(t *testing.T) (*clanksync.Server, *memSyncStore, *blobstore.Memory) {
	t.Helper()
	store := newMemSyncStore()
	mem := blobstore.NewMemory()
	t.Cleanup(mem.Close)
	srv, err := clanksync.NewServer(clanksync.Config{Store: store, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, store, mem
}

// seedUser populates a user with worktrees+checkpoints, a head-bundle row, and
// the matching object-store blobs under "<userID>/".
func seedUser(t *testing.T, store *memSyncStore, mem *blobstore.Memory, userID, worktreeID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.InsertWorktree(ctx, clanksync.Worktree{ID: worktreeID, UserID: userID, DisplayName: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCheckpoint(ctx, clanksync.Checkpoint{ID: worktreeID + "-ck", WorktreeID: worktreeID}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHeadBundle(ctx, clanksync.HeadBundle{UserID: userID, TipSHA: "deadbeef", BlobKey: userID + "/heads/deadbeef.bundle"}); err != nil {
		t.Fatal(err)
	}
	mem.Put(userID+"/checkpoints/"+worktreeID+"/"+worktreeID+"-ck/manifest.json", []byte("x"))
	mem.Put(userID+"/heads/deadbeef.bundle", []byte("y"))
}

func keysWithPrefix(keys []string, prefix string) int {
	n := 0
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// TestPurgeUser_RemovesAllUserData is the headline GDPR test: purging user A
// erases A's worktrees, checkpoints, head bundles, and every "A/" blob, while
// user B's rows and blobs are untouched (tenancy isolation).
func TestPurgeUser_RemovesAllUserData(t *testing.T) {
	t.Parallel()
	srv, store, mem := newPurgeServer(t)
	ctx := context.Background()
	seedUser(t, store, mem, "user-A", "wA1")
	seedUser(t, store, mem, "user-A", "wA2")
	seedUser(t, store, mem, "user-B", "wB1")

	if err := srv.PurgeUser(ctx, "user-A"); err != nil {
		t.Fatalf("PurgeUser: %v", err)
	}

	if wts, _ := store.ListWorktreesByUser(ctx, "user-A"); len(wts) != 0 {
		t.Fatalf("user-A worktrees=%d after purge, want 0", len(wts))
	}
	if cps, _ := store.ListCheckpointsByWorktree(ctx, "wA1", 100); len(cps) != 0 {
		t.Fatalf("user-A checkpoints survived purge: %d", len(cps))
	}
	if _, err := store.GetHeadBundle(ctx, "user-A", "deadbeef"); err == nil {
		t.Fatal("user-A head bundle survived purge")
	}
	if n := keysWithPrefix(mem.Keys(), "user-A/"); n != 0 {
		t.Fatalf("user-A blobs=%d after purge, want 0", n)
	}

	// Tenancy: user-B fully intact.
	if wts, _ := store.ListWorktreesByUser(ctx, "user-B"); len(wts) != 1 {
		t.Fatalf("user-B worktrees=%d, want 1 (purge bled across tenants)", len(wts))
	}
	if _, err := store.GetHeadBundle(ctx, "user-B", "deadbeef"); err != nil {
		t.Fatalf("user-B head bundle deleted by user-A purge: %v", err)
	}
	if n := keysWithPrefix(mem.Keys(), "user-B/"); n != 2 {
		t.Fatalf("user-B blobs=%d, want 2 (purge bled across tenants)", n)
	}
}

// TestPurgeUser_Idempotent: a second purge is a clean no-op.
func TestPurgeUser_Idempotent(t *testing.T) {
	t.Parallel()
	srv, store, mem := newPurgeServer(t)
	ctx := context.Background()
	seedUser(t, store, mem, "user-A", "wA1")

	if err := srv.PurgeUser(ctx, "user-A"); err != nil {
		t.Fatalf("first PurgeUser: %v", err)
	}
	if err := srv.PurgeUser(ctx, "user-A"); err != nil {
		t.Fatalf("second PurgeUser (should be no-op): %v", err)
	}
}

// TestPurgeUser_NoData: purging a user with nothing stored succeeds.
func TestPurgeUser_NoData(t *testing.T) {
	t.Parallel()
	srv, _, _ := newPurgeServer(t)
	if err := srv.PurgeUser(context.Background(), "ghost"); err != nil {
		t.Fatalf("PurgeUser on empty user: %v", err)
	}
}

// TestPurgeUser_RejectsInvalidUserID: an empty userID would form an empty
// blob prefix that sweeps the whole bucket — PurgeUser must refuse it.
func TestPurgeUser_RejectsInvalidUserID(t *testing.T) {
	t.Parallel()
	srv, _, _ := newPurgeServer(t)
	if err := srv.PurgeUser(context.Background(), ""); err == nil {
		t.Fatal("PurgeUser(\"\") should error, not sweep the whole bucket")
	}
}
