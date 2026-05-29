package syncclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	clanksync "github.com/acksell/clank/pkg/sync"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// TestPushCheckpoint_OwnerMismatchReturnsTypedErr pins the typed-error
// contract the CLI's runPushNoMigrate branch depends on: when the sync
// server returns 403 with the OwnerMismatchSentinel in the body, the
// client must surface that as a wrapped ErrOwnerMismatch so the caller
// can branch on errors.Is without HTTP-status sniffing or substring
// parsing.
func TestPushCheckpoint_OwnerMismatchReturnsTypedErr(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror the real ownerMismatchMessage body shape so the
		// client's substring detection has something realistic to
		// match against.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(clanksync.OwnerMismatchSentinel + ": worktree is owned by remote. Run `clank pull -m` to keep remote changes, or `clank push -m --discard-remote` to discard them and push your local state.\n"))
	}))
	defer srv.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL, AuthToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")

	_, pushErr := cli.PushCheckpoint(ctx, "wt-X", repo)
	if pushErr == nil {
		t.Fatal("expected PushCheckpoint to fail; got nil")
	}
	if !errors.Is(pushErr, syncclient.ErrOwnerMismatch) {
		t.Fatalf("expected errors.Is(err, ErrOwnerMismatch); got %v", pushErr)
	}
}

// TestPostJSON_NonOwnerMismatch403IsPlainError guards against the
// typed-error path swallowing every 403: only bodies carrying the
// owner-mismatch sentinel should wrap as ErrOwnerMismatch. Other 403s
// (cross-tenant, sprite missing HostStore, etc.) must remain plain
// errors so they don't get misrouted by callers that branch on
// ErrOwnerMismatch.
func TestPostJSON_NonOwnerMismatch403IsPlainError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden\n"))
	}))
	defer srv.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL, AuthToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}

	_, regErr := cli.RegisterWorktree(ctx, "foo")
	if regErr == nil {
		t.Fatal("expected RegisterWorktree to fail; got nil")
	}
	if errors.Is(regErr, syncclient.ErrOwnerMismatch) {
		t.Fatalf("plain 403 should not surface as ErrOwnerMismatch; got %v", regErr)
	}
}
