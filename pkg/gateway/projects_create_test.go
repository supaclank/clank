package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acksell/clank/pkg/provisioner"
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
		_ = json.NewEncoder(w).Encode(map[string]string{
			"worktree_id":  s.worktreeID,
			"branch":       "main",
			"worktree_dir": "/work/" + s.worktreeID,
			"display_name": body.Name,
			"origin_repo":  body.Name,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newProjectsGateway builds a gateway with the given template catalog,
// pointing its provisioner at sprite. The host filesystem is the repo
// registry — there is no gateway-side store to wire.
func newProjectsGateway(t *testing.T, sprite *httptest.Server, templates []Template) *httptest.Server {
	t.Helper()
	return newProjectsGatewayWithLogger(t, sprite, templates, nil)
}

// newProjectsGatewayWithLogger is newProjectsGateway with an injectable
// logger, for tests asserting on (or withholding secrets from) log output.
func newProjectsGatewayWithLogger(t *testing.T, sprite *httptest.Server, templates []Template, lg *log.Logger) *httptest.Server {
	t.Helper()
	var ref provisioner.HostRef
	if sprite != nil {
		ref = provisioner.HostRef{URL: sprite.URL}
	}
	prov := &stubProvisioner{ref: ref}
	g, err := NewGateway(Config{Provisioner: prov, Templates: templates}, lg)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), projUser))
	t.Cleanup(gw.Close)
	return gw
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
	gw := newProjectsGateway(t, sprite.server(t), []Template{
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

	// The host's CreateWorktreeResult forwards verbatim.
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["worktree_id"] != "01PROJWT" || out["display_name"] != "my-app" {
		t.Fatalf("unexpected body: %+v", out)
	}
}

func TestCreateProject_UnknownTemplate(t *testing.T) {
	t.Parallel()
	sprite := &scaffoldingSprite{worktreeID: "x"}
	gw := newProjectsGateway(t, sprite.server(t), []Template{
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
	gw := newProjectsGateway(t, (&scaffoldingSprite{}).server(t), nil)
	for _, body := range []string{`{"name":"app"}`, `{"template":"expo"}`, `{}`} {
		resp := postJSON(t, gw.URL+"/v1/projects/create", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestCreateProject_Unauthenticated(t *testing.T) {
	t.Parallel()
	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{ref: provisioner.HostRef{URL: "http://unused"}},
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

func TestListTemplates(t *testing.T) {
	t.Parallel()
	gw := newProjectsGateway(t, (&scaffoldingSprite{}).server(t), []Template{
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

	gw := newProjectsGateway(t, sprite, []Template{
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

// The 5xx body withheld from clients (previous test) must also stay out
// of server logs — it can carry the resolved clone URL, credentials and
// all.
func TestCreateProject_HostErrorDoesNotLeakDetailsInLogs(t *testing.T) {
	t.Parallel()
	const sensitiveURL = "https://token@internal.test/template.git"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "clone template: git clone: "+sensitiveURL+": exit status 128", http.StatusInternalServerError)
	})
	sprite := httptest.NewServer(mux)
	t.Cleanup(sprite.Close)

	var logBuf bytes.Buffer
	gw := newProjectsGatewayWithLogger(t, sprite, []Template{
		{ID: "expo", DisplayName: "Expo", CloneURL: sensitiveURL},
	}, log.New(&logBuf, "", 0))

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"expo","name":"app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(logBuf.String(), sensitiveURL) {
		t.Fatalf("host error details leaked into gateway logs: %q", logBuf.String())
	}
}

// A host non-2xx must arrive at the client VERBATIM (status + typed
// body), not flattened to a generic 502 — the regression the repo-first
// cutover fixed for this route (mirrors projects_import.go's contract).
func TestCreateProject_ForwardsHostErrorVerbatim(t *testing.T) {
	t.Parallel()
	const cloneURL = "https://example.test/templates/expo.git"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_argument","error":"template not found in catalog"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gw := newProjectsGateway(t, srv, []Template{{ID: "expo", DisplayName: "Expo", CloneURL: cloneURL}})

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"expo","name":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 forwarded verbatim (not 502)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "invalid_argument") {
		t.Errorf("body = %q, want the host's typed error forwarded", body)
	}
}

// A host 401 is the host's OWN auth middleware rejecting the gateway's
// credentials (an infra failure) — never the client's session. Forwarding
// it verbatim would falsely tell the client its gateway session expired,
// so it must mask to 502 like any other 5xx, unlike other 4xx which
// forward verbatim (TestCreateProject_ForwardsHostErrorVerbatim).
func TestCreateProject_MasksHostUnauthorized(t *testing.T) {
	t.Parallel()
	const cloneURL = "https://example.test/templates/expo.git"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gw := newProjectsGateway(t, srv, []Template{{ID: "expo", DisplayName: "Expo", CloneURL: cloneURL}})

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"expo","name":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (host 401 masked, not forwarded)", resp.StatusCode)
	}
}
