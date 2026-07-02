package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// reposSprite stands in for the host's /repos surface, echoing back a
// configured status + body and recording the path it was hit on.
type reposSprite struct {
	gotPath atomic.Value // string
	status  int
	body    string
}

func (s *reposSprite) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The whole point of the new surface: host statuses arrive VERBATIM —
// no 502 flattening. Table over the four routes and several statuses.
func TestReposProxy_ForwardsStatusVerbatim(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		method     string
		path       string
		wantHost   string
		hostStatus int
		hostBody   string
	}{
		{"list ok", http.MethodGet, "/v1/repos", "/repos", http.StatusOK, `{"repos":[]}`},
		{"create created", http.MethodPost, "/v1/repos/acme__api/worktrees", "/repos/acme__api/worktrees", http.StatusCreated, `{"created":true}`},
		{"create idempotent 200", http.MethodPost, "/v1/repos/acme__api/worktrees", "/repos/acme__api/worktrees", http.StatusOK, `{"created":false}`},
		{"repo_not_found stays 404", http.MethodPost, "/v1/repos/nope/worktrees", "/repos/nope/worktrees", http.StatusNotFound, `{"code":"repo_not_found"}`},
		{"branch 404 stays 404", http.MethodGet, "/v1/repos/acme__api/overview", "/repos/acme__api/overview", http.StatusNotFound, `{"code":"not_found"}`},
		{"busy 409 stays 409", http.MethodDelete, "/v1/repos/acme__api", "/repos/acme__api", http.StatusConflict, `{"code":"worktree_busy"}`},
		{"github 403 stays 403", http.MethodPost, "/v1/repos/acme__api/worktrees", "/repos/acme__api/worktrees", http.StatusForbidden, `{"code":"github_not_connected"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sprite := &reposSprite{status: tc.hostStatus, body: tc.hostBody}
			gw := newProjectsGateway(t, sprite.server(t), nil)

			req, err := http.NewRequest(tc.method, gw.URL+tc.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.hostStatus {
				t.Errorf("status = %d, want %d (verbatim)", resp.StatusCode, tc.hostStatus)
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != tc.hostBody {
				t.Errorf("body = %q, want %q (verbatim)", body, tc.hostBody)
			}
			if got := sprite.gotPath.Load(); got != tc.wantHost {
				t.Errorf("host path = %v, want %s", got, tc.wantHost)
			}
		})
	}
}

// A malformed slug (a "/"-smuggled ".." segment, or characters outside
// the host's own allowlist) must be rejected at the gateway boundary —
// never reach the host at all.
func TestReposProxy_RejectsInvalidSlug(t *testing.T) {
	t.Parallel()
	for _, slug := range []string{"foo/../bar", "foo bar", "foo@bar"} {
		t.Run(slug, func(t *testing.T) {
			t.Parallel()
			sprite := &reposSprite{status: http.StatusOK, body: `{}`}
			gw := newProjectsGateway(t, sprite.server(t), nil)

			resp, err := http.Get(gw.URL + "/v1/repos/" + url.PathEscape(slug) + "/overview")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (rejected at gateway)", resp.StatusCode)
			}
			if got := sprite.gotPath.Load(); got != nil {
				t.Errorf("host was hit at %v, want no host call for an invalid slug", got)
			}
		})
	}
}

// A literal "." or ".." path segment (unescaped) never reaches
// validRepoSlug at all — http.ServeMux's own path cleaning rewrites the
// unclean request path before dispatch, so the cleaned path (e.g.
// /v1/overview) can only fall through to the generic host proxy; it can
// never smuggle a traversal into a /repos/{slug} handler. Documents
// that the belt (gateway allowlist) has a suspenders (stdlib routing)
// behind it for this specific case.
func TestReposProxy_DotDotPathCleanedByServeMux(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/repos/../overview", "/v1/repos/./overview"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			sprite := &reposSprite{status: http.StatusOK, body: `{}`}
			gw := newProjectsGateway(t, sprite.server(t), nil)

			resp, err := http.Get(gw.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got, ok := sprite.gotPath.Load().(string); ok && strings.HasPrefix(got, "/repos") {
				t.Errorf("host /repos surface was hit at %q, want the cleaned path to miss every repos handler", got)
			}
		})
	}
}

// ?fetch=1 must ride through to the host.
func TestReposProxy_ForwardsQuery(t *testing.T) {
	t.Parallel()
	var gotQuery atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	gw := newProjectsGateway(t, srv, nil)

	resp, err := http.Get(gw.URL + "/v1/repos/acme__api/overview?fetch=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := gotQuery.Load(); got != "fetch=1" {
		t.Errorf("host query = %v, want fetch=1", got)
	}
}
