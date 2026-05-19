package daemonclient

import (
	"context"
	"errors"
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
