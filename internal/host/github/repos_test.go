package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestListRepositories_TrimsAndAuthenticates(t *testing.T) {
	t.Parallel()
	var gotAuth atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"api","full_name":"acme/api","private":true,"default_branch":"main",
			 "owner":{"login":"acme"},"updated_at":"2026-01-02T03:04:05Z"},
			{"name":"web","full_name":"acme/web","private":false,"default_branch":"trunk",
			 "owner":{"login":"acme"},"updated_at":"2026-01-01T00:00:00Z"}
		]`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	repos, err := m.ListRepositories(context.Background(), "gho_test")
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("len(repos) = %d, want 2", len(repos))
	}
	want := Repo{Owner: "acme", Name: "api", FullName: "acme/api", Private: true, DefaultBranch: "main"}
	got := repos[0]
	got.UpdatedAt = want.UpdatedAt // compared separately below
	if got != want {
		t.Errorf("repos[0] = %+v, want %+v", got, want)
	}
	if repos[0].UpdatedAt.IsZero() {
		t.Error("repos[0].UpdatedAt is zero, want parsed timestamp")
	}
	if a, _ := gotAuth.Load().(string); a != "Bearer gho_test" {
		t.Errorf("Authorization = %q, want Bearer gho_test", a)
	}
}

func TestListRepositories_Paginates(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"name":"b","full_name":"acme/b","owner":{"login":"acme"}}]`))
			return
		}
		// Page 1 points the client to page 2 via the Link header, which
		// go-github parses into resp.NextPage.
		w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next"`, srv.URL))
		_, _ = w.Write([]byte(`[{"name":"a","full_name":"acme/a","owner":{"login":"acme"}}]`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	repos, err := m.ListRepositories(context.Background(), "gho_test")
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("len(repos) = %d, want 2 (both pages)", len(repos))
	}
	if repos[0].Name != "a" || repos[1].Name != "b" {
		t.Errorf("pages not concatenated in order: %q, %q", repos[0].Name, repos[1].Name)
	}
}

func TestAccessToken(t *testing.T) {
	t.Parallel()
	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")

	if _, err := m.AccessToken(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("AccessToken with no creds: err = %v, want ErrNotConnected", err)
	}

	if err := m.Store().Write(Credentials{AccessToken: "gho_abc"}); err != nil {
		t.Fatalf("Store().Write: %v", err)
	}
	tok, err := m.AccessToken()
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "gho_abc" {
		t.Errorf("token = %q, want gho_abc", tok)
	}
}
