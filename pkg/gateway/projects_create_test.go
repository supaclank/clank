package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/supaclank/clank/pkg/provisioner"
)

const projUser = "tester"

// templatesHost stands in for the host's template surface: the catalog
// listing and the create endpoint, recording what the proxy forwarded.
type templatesHost struct {
	gotCloneURL atomic.Value // string
	gotName     atomic.Value // string
}

func (h *templatesHost) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /templates", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"display_name":"Expo app","clone_url":"https://templates.example/expo.git","source":"builtin"},
			{"display_name":"acme/tpl","clone_url":"https://github.example/acme/tpl.git","source":"github","description":"Mine"}
		]`))
	})
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			CloneURL string `json:"clone_url"`
			Name     string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.gotCloneURL.Store(body.CloneURL)
		h.gotName.Store(body.Name)
		if body.CloneURL == "" || body.Name == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_argument","error":"clone_url and name are required"}`))
			return
		}
		if strings.Contains(body.CloneURL, "unclonable") {
			// The host's typed, sanitized clone failure — must forward
			// verbatim through the proxy.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"template_clone_failed","error":"host: template clone failed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"worktree_id": "01PROJWT", "display_name": body.Name})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newProjectsGateway wires a gateway whose provisioner points at the
// stub host. Templates are entirely the host's business now — there is
// no gateway-side catalog to configure.
func newProjectsGateway(t *testing.T, sprite *httptest.Server) *httptest.Server {
	t.Helper()
	var ref provisioner.HostRef
	if sprite != nil {
		ref = provisioner.HostRef{URL: sprite.URL}
	}
	prov := &stubProvisioner{ref: ref}
	g, err := NewGateway(Config{Provisioner: prov}, nil)
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

// The gateway is a pure proxy for the catalog: entries — clone URLs
// included — pass through verbatim, because clients pick an entry and
// send its clone_url back to create.
func TestListTemplates_ProxiesHostCatalog(t *testing.T) {
	t.Parallel()
	h := &templatesHost{}
	gw := newProjectsGateway(t, h.server(t))

	resp, err := http.Get(gw.URL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var got []struct {
		DisplayName string `json:"display_name"`
		CloneURL    string `json:"clone_url"`
		Source      string `json:"source"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(got) != 2 || got[0].CloneURL != "https://templates.example/expo.git" || got[1].Source != "github" {
		t.Fatalf("catalog not forwarded verbatim: %+v", got)
	}
}

func TestCreateProject_ForwardsCloneURL(t *testing.T) {
	t.Parallel()
	h := &templatesHost{}
	gw := newProjectsGateway(t, h.server(t))

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"clone_url":"https://templates.example/expo.git","name":"my-app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := h.gotCloneURL.Load(); got != "https://templates.example/expo.git" {
		t.Fatalf("host clone_url = %v", got)
	}
	if got := h.gotName.Load(); got != "my-app" {
		t.Fatalf("host name = %v", got)
	}
}

// The host's typed errors — the whole point of the sanitized
// template_clone_failed design — must reach clients verbatim.
func TestCreateProject_ForwardsTypedHostErrors(t *testing.T) {
	t.Parallel()
	h := &templatesHost{}
	gw := newProjectsGateway(t, h.server(t))

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"clone_url":"https://templates.example/unclonable.git","name":"x"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "template_clone_failed") {
		t.Fatalf("typed error not forwarded: %s", body)
	}
}

// Without an authenticated principal, the proxy answers 401 before
// touching the provisioner or the host.
func TestCreateProject_Unauthenticated(t *testing.T) {
	t.Parallel()
	h := &templatesHost{}
	g, err := NewGateway(Config{Provisioner: &stubProvisioner{ref: provisioner.HostRef{URL: h.server(t).URL}}}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	// No auth middleware → no Principal in context.
	gw := httptest.NewServer(g.Handler())
	t.Cleanup(gw.Close)

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"clone_url":"https://x.test/y.git","name":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if h.gotCloneURL.Load() != nil {
		t.Fatal("unauthenticated request reached the host")
	}
}
