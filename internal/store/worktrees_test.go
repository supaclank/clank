package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
)

func TestWorktrees_InsertGetList(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	w := store.Worktree{
		ID:          "wt-1",
		UserID:      "user-A",
		DisplayName: "myrepo (main)",
	}
	if err := s.InsertWorktree(ctx, w); err != nil {
		t.Fatalf("InsertWorktree: %v", err)
	}

	got, err := s.GetWorktreeByID(ctx, "wt-1")
	if err != nil {
		t.Fatalf("GetWorktreeByID: %v", err)
	}
	if got.UserID != "user-A" || got.DisplayName != "myrepo (main)" {
		t.Fatalf("worktree round-trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not populated: %+v", got)
	}

	list, err := s.ListWorktreesByUser(ctx, "user-A")
	if err != nil {
		t.Fatalf("ListWorktreesByUser: %v", err)
	}
	if len(list) != 1 || list[0].ID != "wt-1" {
		t.Fatalf("ListWorktreesByUser: want 1 row id=wt-1, got %+v", list)
	}

	if _, err := s.GetWorktreeByID(ctx, "missing"); !errors.Is(err, store.ErrWorktreeNotFound) {
		t.Fatalf("expected ErrWorktreeNotFound, got %v", err)
	}
}

func TestHeadBundles_InsertGetIdempotent(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	if _, err := s.GetHeadBundle(ctx, "u", "missing"); !errors.Is(err, store.ErrHeadBundleNotFound) {
		t.Fatalf("expected ErrHeadBundleNotFound, got %v", err)
	}

	if err := s.InsertHeadBundle(ctx, store.HeadBundle{UserID: "u", TipSHA: "abc", BaseSHA: "", BlobKey: "u/heads/abc.bundle"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetHeadBundle(ctx, "u", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSHA != "" || got.BlobKey != "u/heads/abc.bundle" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// INSERT OR IGNORE: a second insert for the same (user, tip) is a
	// no-op — the first stored bundle (and its base) wins.
	if err := s.InsertHeadBundle(ctx, store.HeadBundle{UserID: "u", TipSHA: "abc", BaseSHA: "DIFFERENT", BlobKey: "x"}); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetHeadBundle(ctx, "u", "abc")
	if got2.BaseSHA != "" || got2.BlobKey != "u/heads/abc.bundle" {
		t.Fatalf("second insert should be ignored, got %+v", got2)
	}

	// Same tip SHA under a different user is a distinct row.
	if _, err := s.GetHeadBundle(ctx, "other", "abc"); !errors.Is(err, store.ErrHeadBundleNotFound) {
		t.Fatalf("tip must be scoped per user, got %v", err)
	}
}

func TestCheckpoints_InsertAndPointerAdvance(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	if err := s.InsertWorktree(ctx, store.Worktree{
		ID: "wt", UserID: "u", DisplayName: "r",
	}); err != nil {
		t.Fatal(err)
	}

	c := store.Checkpoint{
		ID:                "ck-1",
		WorktreeID:        "wt",
		HeadCommit:        "deadbeef",
		HeadRef:           "main",
		IndexTree:         "1111",
		WorktreeTree:      "2222",
		UncommittedCommit: "3333",
		CreatedBy:         "laptop:dev-1",
	}
	if err := s.InsertCheckpoint(ctx, c); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	got, err := s.GetCheckpointByID(ctx, "ck-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadCommit != "deadbeef" || got.UncommittedCommit != "3333" {
		t.Fatalf("checkpoint round-trip mismatch: %+v", got)
	}
	if !got.UploadedAt.IsZero() {
		t.Fatalf("UploadedAt should be zero before MarkCheckpointUploaded, got %v", got.UploadedAt)
	}

	when := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkCheckpointUploaded(ctx, "ck-1", when); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetCheckpointByID(ctx, "ck-1")
	if !got.UploadedAt.Equal(when) {
		t.Fatalf("UploadedAt mismatch: want %v got %v", when, got.UploadedAt)
	}

	// Pointer advance.
	if err := s.UpdateWorktreePointer(ctx, "wt", "ck-1"); err != nil {
		t.Fatal(err)
	}
	wt, _ := s.GetWorktreeByID(ctx, "wt")
	if wt.LatestSyncedCheckpoint != "ck-1" {
		t.Fatalf("pointer advance failed: %q", wt.LatestSyncedCheckpoint)
	}
}
