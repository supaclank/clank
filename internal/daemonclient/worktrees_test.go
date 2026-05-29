package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acksell/clank/internal/cloud"
)

// TestListWorktrees_UnauthorizedWraps pins the contract that a 401 from
// the gateway surfaces as cloud.ErrUnauthorized — callers (notably
// `clank status`) use errors.Is on this to route to the "Not signed
// in" branch instead of "remote unreachable".
func TestListWorktrees_UnauthorizedWraps(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	cli := NewTCPClient(srv.URL, "stale-token")
	_, err := cli.ListWorktrees(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !errors.Is(err, cloud.ErrUnauthorized) {
		t.Errorf("expected errors.Is(err, cloud.ErrUnauthorized); got %v", err)
	}
}

// TestGetWorktree_UnauthorizedWraps mirrors the contract for the single
// worktree endpoint.
func TestGetWorktree_UnauthorizedWraps(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	cli := NewTCPClient(srv.URL, "stale-token")
	_, err := cli.GetWorktree(context.Background(), "01ABC")
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !errors.Is(err, cloud.ErrUnauthorized) {
		t.Errorf("expected errors.Is(err, cloud.ErrUnauthorized); got %v", err)
	}
}

// TestReclaimWorktree_HappyPath pins the request contract used by the
// new `clank push -m --discard-remote` reclaim path: POST to
// /v1/worktrees/{id}/owner with {to_kind:"local", to_id:"",
// expected_owner_id:"<input>"}, body parsed back as WorktreeInfo with
// owner_kind=local.
func TestReclaimWorktree_HappyPath(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(WorktreeInfo{
			ID:          "wt-1",
			UserID:      "user-A",
			DisplayName: "demo",
			OwnerKind:   string(OwnerKindLocal),
			OwnerID:     "",
		})
	}))
	defer srv.Close()

	cli := NewTCPClient(srv.URL, "tok")
	got, err := cli.ReclaimWorktree(context.Background(), "wt-1", "host-X")
	if err != nil {
		t.Fatalf("ReclaimWorktree: %v", err)
	}
	if got.OwnerKind != string(OwnerKindLocal) {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, OwnerKindLocal)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/worktrees/wt-1/owner" {
		t.Fatalf("path = %q, want /v1/worktrees/wt-1/owner", gotPath)
	}
	if gotBody["to_kind"] != string(OwnerKindLocal) {
		t.Fatalf("to_kind = %q, want %q", gotBody["to_kind"], OwnerKindLocal)
	}
	if gotBody["to_id"] != "" {
		t.Fatalf("to_id = %q, want empty for local kind", gotBody["to_id"])
	}
	if gotBody["expected_owner_id"] != "host-X" {
		t.Fatalf("expected_owner_id = %q, want %q", gotBody["expected_owner_id"], "host-X")
	}
}

// TestReclaimWorktree_OwnerConflict pins the 409 → ErrOwnerConflict
// mapping. The CLI's --discard-remote branch surfaces this as "the
// worktree changed under you — re-run" rather than silently retrying.
func TestReclaimWorktree_OwnerConflict(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "owner mismatch (concurrent migration?)", http.StatusConflict)
	}))
	defer srv.Close()

	cli := NewTCPClient(srv.URL, "tok")
	_, err := cli.ReclaimWorktree(context.Background(), "wt-1", "host-STALE")
	if err == nil {
		t.Fatal("expected error for 409 response, got nil")
	}
	if !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("expected errors.Is(err, ErrOwnerConflict); got %v", err)
	}
}

// TestReclaimWorktree_UnauthorizedWraps locks the same 401 contract
// that GetWorktree honors — keeps reclaim consistent with the existing
// worktree-endpoint family so callers can branch on cloud.ErrUnauthorized.
func TestReclaimWorktree_UnauthorizedWraps(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	cli := NewTCPClient(srv.URL, "stale-token")
	_, err := cli.ReclaimWorktree(context.Background(), "wt-1", "host-X")
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !errors.Is(err, cloud.ErrUnauthorized) {
		t.Fatalf("expected errors.Is(err, cloud.ErrUnauthorized); got %v", err)
	}
}

// TestReclaimWorktree_NotFound locks the 404 → ErrWorktreeNotFound
// mapping, consistent with GetWorktree's contract.
func TestReclaimWorktree_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cli := NewTCPClient(srv.URL, "tok")
	_, err := cli.ReclaimWorktree(context.Background(), "wt-missing", "host-X")
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
	if !errors.Is(err, ErrWorktreeNotFound) {
		t.Fatalf("expected errors.Is(err, ErrWorktreeNotFound); got %v", err)
	}
}
