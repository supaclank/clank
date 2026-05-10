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
		OwnerKind:   store.OwnerKindLocal,
		OwnerID:     "dev-1",
	}
	if err := s.InsertWorktree(ctx, w); err != nil {
		t.Fatalf("InsertWorktree: %v", err)
	}

	got, err := s.GetWorktreeByID(ctx, "wt-1")
	if err != nil {
		t.Fatalf("GetWorktreeByID: %v", err)
	}
	if got.UserID != "user-A" || got.OwnerID != "dev-1" || got.OwnerKind != store.OwnerKindLocal {
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

func TestWorktrees_OwnerTransferAtomic(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	if err := s.InsertWorktree(ctx, store.Worktree{
		ID: "wt", UserID: "u", DisplayName: "r", OwnerKind: store.OwnerKindLocal, OwnerID: "dev-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Successful transfer: caller knows the current owner.
	if err := s.UpdateWorktreeOwner(ctx, "wt", store.OwnerKindLocal, "dev-1", store.OwnerKindRemote, "sprite-X"); err != nil {
		t.Fatalf("UpdateWorktreeOwner (happy): %v", err)
	}
	got, err := s.GetWorktreeByID(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerKind != store.OwnerKindRemote || got.OwnerID != "sprite-X" {
		t.Fatalf("post-transfer owner mismatch: %+v", got)
	}

	// Stale transfer: caller's expected owner is the old laptop ID.
	// Should return ErrOwnerMismatch and leave the row unchanged.
	err = s.UpdateWorktreeOwner(ctx, "wt", store.OwnerKindLocal, "dev-1", store.OwnerKindLocal, "dev-1")
	if !errors.Is(err, store.ErrOwnerMismatch) {
		t.Fatalf("expected ErrOwnerMismatch on stale transfer, got %v", err)
	}
	got, _ = s.GetWorktreeByID(ctx, "wt")
	if got.OwnerKind != store.OwnerKindRemote || got.OwnerID != "sprite-X" {
		t.Fatalf("row mutated by failed transfer: %+v", got)
	}
}

// TestWorktrees_OwnerTransferRejectsKindMismatch pins the full-tuple
// CAS: even when (id, owner_id) match, the wrong owner_kind must
// still surface ErrOwnerMismatch.
func TestWorktrees_OwnerTransferRejectsKindMismatch(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))
	ctx := context.Background()

	// Construct a row owned by remote/"shared-id".
	if err := s.InsertWorktree(ctx, store.Worktree{
		ID: "wt", UserID: "u", DisplayName: "r", OwnerKind: store.OwnerKindRemote, OwnerID: "shared-id",
	}); err != nil {
		t.Fatal(err)
	}

	// Caller passes the right id and matching owner_id but the wrong
	// kind. Without the kind in the CAS, this would have hijacked the
	// row.
	err := s.UpdateWorktreeOwner(ctx, "wt", store.OwnerKindLocal, "shared-id", store.OwnerKindLocal, "imposter")
	if !errors.Is(err, store.ErrOwnerMismatch) {
		t.Fatalf("expected ErrOwnerMismatch on kind mismatch, got %v", err)
	}
	got, _ := s.GetWorktreeByID(ctx, "wt")
	if got.OwnerKind != store.OwnerKindRemote || got.OwnerID != "shared-id" {
		t.Fatalf("row mutated by cross-kind transfer attempt: %+v", got)
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
		IncrementalCommit: "3333",
		CreatedBy:         "laptop:dev-1",
	}
	if err := s.InsertCheckpoint(ctx, c); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	got, err := s.GetCheckpointByID(ctx, "ck-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadCommit != "deadbeef" || got.IncrementalCommit != "3333" {
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
