package sync_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/auth"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// headerPrincipalMiddleware pulls UserID from X-Test-User-Id and
// injects it as an auth.Principal so each request can assert as a
// different user without a real token issuer.
func headerPrincipalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := r.Header.Get("X-Test-User-Id")
		if u == "" {
			http.Error(w, "missing X-Test-User-Id", http.StatusUnauthorized)
			return
		}
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{UserID: u})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TestRemoteCaller_RejectedWhenHostStoreUnset pins the
// belt-and-suspenders behavior: a caller presenting X-Clank-Host-Id
// (sprite kind) must be rejected with 403 when HostStore is nil,
// instead of silently bypassing the cross-tenant guard.
func TestRemoteCaller_RejectedWhenHostStoreUnset(t *testing.T) {
	t.Parallel()

	dbPath := tempDBPathHelper(t)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mem := storage.NewMemory()
	t.Cleanup(mem.Close)

	srv, err := clanksync.NewServer(clanksync.Config{
		Store:      st,
		Storage:    mem,
		PresignTTL: time.Minute,
		// HostStore deliberately nil — production deployment without
		// the cross-tenant store should still refuse remote callers.
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(headerPrincipalMiddleware(srv.Handler()))
	t.Cleanup(httpSrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seed a worktree as a normal laptop caller so there's something to read.
	worktreeID, err := callRegisterWorktree(ctx, httpSrv.URL, "user-A", "wt")
	if err != nil {
		t.Fatalf("register worktree: %v", err)
	}

	// Read it as a sprite caller (X-Clank-Host-Id). Must 403, not 200.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpSrv.URL+"/v1/worktrees/"+worktreeID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User-Id", "user-A")
	req.Header.Set("X-Clank-Host-Id", "sprite-imposter")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 forbidden for remote caller without HostStore, got %d", resp.StatusCode)
	}
}

func callRegisterWorktree(ctx context.Context, baseURL, userID, displayName string) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	body := map[string]string{"display_name": displayName}
	if err := callJSON(ctx, http.MethodPost, baseURL+"/v1/worktrees", userID, body, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", errors.New("empty worktree id")
	}
	return resp.ID, nil
}

func callJSON(ctx context.Context, method, url, userID string, body any, into any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(buf)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-Id", userID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d", method, url, resp.StatusCode)
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
	}
	return nil
}

func tempDBPathHelper(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/test.db"
}
