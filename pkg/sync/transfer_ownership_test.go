package sync_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	clanksync "github.com/acksell/clank/pkg/sync"
)

// TestTransferOwnership_LaptopReclaimsRemoteOwnedWorktree pins the
// happy path the new `clank push -m --discard-remote` flow takes: a
// laptop claiming back a worktree the sprite currently owns. The body
// has an empty to_id (per-user laptop ownership has no device ID), so
// this also regression-tests the validation tweak that made empty
// to_id legal for to_kind=local.
func TestTransferOwnership_LaptopReclaimsRemoteOwnedWorktree(t *testing.T) {
	t.Parallel()
	httpSrv, store, _ := newTestServer(t)

	// Register as laptop, then flip to remote-owned via the store so
	// the worktree starts out claimed by sprite-host-X.
	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "myrepo",
	})
	worktreeID := wt["id"].(string)
	if err := store.UpdateWorktreeOwner(
		context.Background(), worktreeID,
		clanksync.OwnerKindLocal, "",
		clanksync.OwnerKindRemote, "host-X",
	); err != nil {
		t.Fatalf("seed remote-owned: %v", err)
	}

	// Laptop reclaims. expected_owner_id must reflect the current
	// remote-owner ID for the optimistic-concurrency guard to pass.
	got := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees/"+worktreeID+"/owner", map[string]string{
		"to_kind":           string(clanksync.OwnerKindLocal),
		"to_id":             "",
		"expected_owner_id": "host-X",
	})
	if got["owner_kind"] != string(clanksync.OwnerKindLocal) {
		t.Fatalf("owner_kind = %v, want %q", got["owner_kind"], clanksync.OwnerKindLocal)
	}
	if got["owner_id"] != "" {
		t.Fatalf("owner_id = %v, want empty for local", got["owner_id"])
	}

	// Store should reflect the flip.
	updated, _ := store.GetWorktreeByID(context.Background(), worktreeID)
	if updated.OwnerKind != clanksync.OwnerKindLocal || updated.OwnerID != "" {
		t.Fatalf("store row not flipped: %+v", updated)
	}
}

// TestTransferOwnership_RemoteStillRequiresToID locks the remaining
// validation: sprite ownership is per-device, so an empty to_id when
// claiming the worktree for to_kind=remote is still a 400.
func TestTransferOwnership_RemoteStillRequiresToID(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)
	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "r",
	})
	worktreeID := wt["id"].(string)

	body := mustPostExpectStatus(t, httpSrv.URL+"/v1/worktrees/"+worktreeID+"/owner", map[string]string{
		"to_kind": string(clanksync.OwnerKindRemote),
		"to_id":   "",
	}, http.StatusBadRequest)
	if !strings.Contains(string(body), "to_id") {
		t.Fatalf("400 body should name the to_id field, got %q", body)
	}
}

// TestTransferOwnership_StaleExpectedOwnerIDReturns409 pins the
// optimistic-concurrency contract: if the caller's expected_owner_id
// no longer matches reality (someone else reclaimed first), the server
// must return 409 so the caller can re-read and retry rather than
// silently proceeding against a stale view.
func TestTransferOwnership_StaleExpectedOwnerIDReturns409(t *testing.T) {
	t.Parallel()
	httpSrv, store, _ := newTestServer(t)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "r",
	})
	worktreeID := wt["id"].(string)
	if err := store.UpdateWorktreeOwner(
		context.Background(), worktreeID,
		clanksync.OwnerKindLocal, "",
		clanksync.OwnerKindRemote, "host-X",
	); err != nil {
		t.Fatalf("seed remote-owned: %v", err)
	}

	mustPostExpectStatus(t, httpSrv.URL+"/v1/worktrees/"+worktreeID+"/owner", map[string]string{
		"to_kind":           string(clanksync.OwnerKindLocal),
		"to_id":             "",
		"expected_owner_id": "host-STALE",
	}, http.StatusConflict)
}

// TestCheckpointOnRemoteOwnedWorktree_403BodyMentionsBothReclaimPaths
// pins the softened wording: a laptop POSTing a checkpoint to a
// sprite-owned worktree gets a 403 whose body names BOTH reclaim paths
// (`clank pull -m` to keep remote changes, `clank push -m
// --discard-remote` to discard them). This is the message the CLI's
// runPushNoMigrate branch checks for to render the styled options
// block — see internal/cli/clankcli/push.go.
func TestCheckpointOnRemoteOwnedWorktree_403BodyMentionsBothReclaimPaths(t *testing.T) {
	t.Parallel()
	httpSrv, store, _ := newTestServer(t)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "r",
	})
	worktreeID := wt["id"].(string)
	if err := store.UpdateWorktreeOwner(
		context.Background(), worktreeID,
		clanksync.OwnerKindLocal, "",
		clanksync.OwnerKindRemote, "host-X",
	); err != nil {
		t.Fatalf("seed remote-owned: %v", err)
	}

	body := mustPostExpectStatus(t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        "x",
		"index_tree":         "x",
		"worktree_tree":      "x",
		"incremental_commit": "x",
	}, http.StatusForbidden)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, clanksync.OwnerMismatchSentinel) {
		t.Fatalf("403 body missing owner-mismatch sentinel %q: %q", clanksync.OwnerMismatchSentinel, bodyStr)
	}
	if !strings.Contains(bodyStr, "clank pull -m") {
		t.Fatalf("403 body should mention `clank pull -m`, got %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "clank push -m --discard-remote") {
		t.Fatalf("403 body should mention `clank push -m --discard-remote`, got %q", bodyStr)
	}
}
