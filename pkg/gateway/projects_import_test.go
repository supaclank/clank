package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acksell/clank/pkg/provisioner"
)

// importSprite stands in for the host's POST /projects/import. It records
// the owner/repo it received and returns either a CreateWorktreeResult or,
// when errStatus is set, a typed error (e.g. github_not_connected).
type importSprite struct {
	gotOwner   atomic.Value // string
	gotRepo    atomic.Value // string
	calls      int32
	worktreeID string

	errStatus int    // 0 → respond 201 with a result
	errBody   string // body for the error response
}

func (s *importSprite) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/import", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.calls, 1)
		var body struct {
			Owner string `json:"owner"`
			Repo  string `json:"repo"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.gotOwner.Store(body.Owner)
		s.gotRepo.Store(body.Repo)

		if s.errStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.errStatus)
			_, _ = w.Write([]byte(s.errBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createWorktreeResponse{
			WorktreeID:  s.worktreeID,
			Branch:      "main",
			WorktreeDir: "/work/" + s.worktreeID,
			DisplayName: body.Repo,
			OriginRepo:  body.Owner + "/" + body.Repo,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestImportProject_HappyPath(t *testing.T) {
	t.Parallel()
	sprite := &importSprite{worktreeID: "01IMPORTWT"}
	gw, st := newProjectsGateway(t, sprite.server(t), nil)

	resp := postJSON(t, gw.URL+"/v1/projects/import", `{"owner":"acme","repo":"api"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	// The host received the discrete owner/repo, not a URL.
	if got := sprite.gotOwner.Load(); got != "acme" {
		t.Fatalf("host owner = %v, want acme", got)
	}
	if got := sprite.gotRepo.Load(); got != "api" {
		t.Fatalf("host repo = %v, want api", got)
	}

	// The worktree row was persisted with the GitHub-derived label.
	wt, err := st.GetWorktreeByID(context.Background(), "01IMPORTWT")
	if err != nil {
		t.Fatalf("worktree row not persisted: %v", err)
	}
	if wt.UserID != projUser || wt.DisplayName != "api" || wt.OriginRepo != "acme/api" {
		t.Fatalf("unexpected row: %+v", wt)
	}
}

func TestImportProject_NotConnectedForwarded(t *testing.T) {
	t.Parallel()
	sprite := &importSprite{
		worktreeID: "01SHOULDNOTEXIST",
		errStatus:  http.StatusConflict,
		errBody:    `{"code":"github_not_connected","error":"github: not connected"}`,
	}
	gw, st := newProjectsGateway(t, sprite.server(t), nil)

	resp := postJSON(t, gw.URL+"/v1/projects/import", `{"owner":"acme","repo":"api"}`)
	defer resp.Body.Close()

	// The host's typed error must reach the client verbatim, not collapse
	// to a generic 502 — the mobile app branches on github_not_connected.
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (host error forwarded)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "github_not_connected") {
		t.Fatalf("error code not forwarded: %q", body)
	}

	// No worktree row should be registered on failure.
	if _, err := st.GetWorktreeByID(context.Background(), "01SHOULDNOTEXIST"); err == nil {
		t.Fatal("worktree row persisted despite host error")
	}
}

func TestImportProject_MissingFields(t *testing.T) {
	t.Parallel()
	sprite := &importSprite{worktreeID: "x"}
	gw, _ := newProjectsGateway(t, sprite.server(t), nil)
	for _, body := range []string{`{"owner":"acme"}`, `{"repo":"api"}`, `{}`} {
		resp := postJSON(t, gw.URL+"/v1/projects/import", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if n := atomic.LoadInt32(&sprite.calls); n != 0 {
		t.Fatalf("host calls = %d, want 0 (validation precedes the host)", n)
	}
}

func TestImportProject_SyncUnconfigured(t *testing.T) {
	t.Parallel()
	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{ref: provisioner.HostRef{URL: "http://unused"}},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), projUser))
	t.Cleanup(gw.Close)

	resp := postJSON(t, gw.URL+"/v1/projects/import", `{"owner":"acme","repo":"api"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when Sync unconfigured", resp.StatusCode)
	}
}
