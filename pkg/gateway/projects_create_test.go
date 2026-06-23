package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
)

const projUser = "tester"

// scaffoldingSprite stands in for the host's POST /projects/create. It
// records the clone_url it received and returns a CreateWorktreeResult.
type scaffoldingSprite struct {
	gotCloneURL atomic.Value // string
	gotName     atomic.Value // string
	calls       int32
	worktreeID  string
}

func (s *scaffoldingSprite) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.calls, 1)
		var body struct {
			CloneURL string `json:"clone_url"`
			Name     string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.gotCloneURL.Store(body.CloneURL)
		s.gotName.Store(body.Name)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createWorktreeResponse{
			WorktreeID:  s.worktreeID,
			Branch:      "main",
			WorktreeDir: "/work/" + s.worktreeID,
			DisplayName: body.Name,
			OriginRepo:  body.Name,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newProjectsGateway builds a gateway with the given template catalog,
// pointing its provisioner at sprite, backed by a real SQLite sync store.
func newProjectsGateway(t *testing.T, sprite *httptest.Server, templates []Template) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mem := blobstore.NewMemory()
	t.Cleanup(mem.Close)
	syncSrv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	var ref provisioner.HostRef
	if sprite != nil {
		ref = provisioner.HostRef{URL: sprite.URL}
	}
	prov := &stubProvisioner{ref: ref}
	g, err := NewGateway(Config{Provisioner: prov, Sync: syncSrv, Templates: templates}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), projUser))
	t.Cleanup(gw.Close)
	return gw, st
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCreateProject_HappyPath(t *testing.T) {
	t.Parallel()
	const cloneURL = "https://example.test/templates/expo.git"
	sprite := &scaffoldingSprite{worktreeID: "01PROJWT"}
	gw, st := newProjectsGateway(t, sprite.server(t), []Template{
		{ID: "expo", DisplayName: "Expo app", CloneURL: cloneURL},
	})

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"expo","name":"my-app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	// The host received the resolved clone URL, never the template id.
	if got := sprite.gotCloneURL.Load(); got != cloneURL {
		t.Fatalf("host clone_url = %v, want %q", got, cloneURL)
	}

	// The worktree row was persisted for this user.
	wt, err := st.GetWorktreeByID(context.Background(), "01PROJWT")
	if err != nil {
		t.Fatalf("worktree row not persisted: %v", err)
	}
	if wt.UserID != projUser || wt.DisplayName != "my-app" || wt.OriginRepo != "my-app" {
		t.Fatalf("unexpected row: %+v", wt)
	}
}

func TestCreateProject_UnknownTemplate(t *testing.T) {
	t.Parallel()
	sprite := &scaffoldingSprite{worktreeID: "x"}
	gw, _ := newProjectsGateway(t, sprite.server(t), []Template{
		{ID: "expo", DisplayName: "Expo", CloneURL: "https://example.test/expo.git"},
	})

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"nope","name":"app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown template", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&sprite.calls); n != 0 {
		t.Fatalf("host calls = %d, want 0 (resolution fails before the host)", n)
	}
}

func TestCreateProject_MissingFields(t *testing.T) {
	t.Parallel()
	gw, _ := newProjectsGateway(t, (&scaffoldingSprite{}).server(t), nil)
	for _, body := range []string{`{"name":"app"}`, `{"template":"expo"}`, `{}`} {
		resp := postJSON(t, gw.URL+"/v1/projects/create", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestCreateProject_SyncUnconfigured(t *testing.T) {
	t.Parallel()
	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{ref: provisioner.HostRef{URL: "http://unused"}},
		Templates:   []Template{{ID: "expo", DisplayName: "Expo", CloneURL: "https://example.test/expo.git"}},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), projUser))
	t.Cleanup(gw.Close)

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"expo","name":"app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when Sync unconfigured", resp.StatusCode)
	}
}

func TestCreateProject_Unauthenticated(t *testing.T) {
	t.Parallel()
	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{ref: provisioner.HostRef{URL: "http://unused"}},
		Sync:        mustSyncServer(t),
		Templates:   []Template{{ID: "expo", DisplayName: "Expo", CloneURL: "https://example.test/expo.git"}},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	// No auth middleware → no Principal in context.
	gw := httptest.NewServer(g.Handler())
	t.Cleanup(gw.Close)

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"expo","name":"app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a principal", resp.StatusCode)
	}
}

// mustSyncServer builds a throwaway sync server backed by SQLite + memory
// storage, for tests that only need a non-nil Sync.
func mustSyncServer(t *testing.T) *clanksync.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mem := blobstore.NewMemory()
	t.Cleanup(mem.Close)
	syncSrv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return syncSrv
}

func TestListTemplates(t *testing.T) {
	t.Parallel()
	gw, _ := newProjectsGateway(t, (&scaffoldingSprite{}).server(t), []Template{
		{ID: "expo", DisplayName: "Expo app", CloneURL: "https://secret.test/expo.git"},
	})

	resp, err := http.Get(gw.URL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0]["id"] != "expo" || out[0]["display_name"] != "Expo app" {
		t.Fatalf("unexpected catalog: %+v", out)
	}
	// Clone URLs must never leak to clients.
	if _, leaked := out[0]["clone_url"]; leaked {
		t.Fatalf("clone_url leaked in catalog: %+v", out[0])
	}
}

func TestListTemplates_Unauthenticated(t *testing.T) {
	t.Parallel()
	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{ref: provisioner.HostRef{URL: "http://unused"}},
		Templates:   []Template{{ID: "expo", DisplayName: "Expo", CloneURL: "https://secret.test/expo.git"}},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(g.Handler())
	t.Cleanup(gw.Close)

	resp, err := http.Get(gw.URL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a principal", resp.StatusCode)
	}
}

func TestCreateProject_HostErrorDoesNotLeakDetails(t *testing.T) {
	t.Parallel()
	const sensitiveURL = "https://token@internal.test/template.git"
	// Sprite that simulates a clone failure — error body includes clone URL detail.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "clone template: git clone: "+sensitiveURL+": exit status 128", http.StatusInternalServerError)
	})
	sprite := httptest.NewServer(mux)
	t.Cleanup(sprite.Close)

	gw, _ := newProjectsGateway(t, sprite, []Template{
		{ID: "expo", DisplayName: "Expo", CloneURL: sensitiveURL},
	})

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"expo","name":"app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), sensitiveURL) {
		t.Fatalf("host error details leaked in gateway 502 body: %q", body)
	}
}
