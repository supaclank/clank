package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestListTemplateRepositories_FiltersAndScopes(t *testing.T) {
	t.Parallel()
	var gotAffiliation atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		gotAffiliation.Store(r.URL.Query().Get("affiliation"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"expo-tpl","full_name":"acme/expo-tpl","is_template":true,"private":false,
			 "default_branch":"main","description":"Expo starter","owner":{"login":"acme"}},
			{"name":"api","full_name":"acme/api","is_template":false,"private":true,
			 "default_branch":"main","owner":{"login":"acme"}},
			{"name":"go-tpl","full_name":"acme/go-tpl","is_template":true,"private":true,
			 "default_branch":"main","owner":{"login":"acme"}}
		]`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	repos, err := m.ListTemplateRepositories(context.Background(), "gho_test")
	if err != nil {
		t.Fatalf("ListTemplateRepositories: %v", err)
	}
	// Owner-only affiliation is the v1 product boundary ("your own
	// templates") — a silent widening here would leak org repos into
	// the picker.
	if got := gotAffiliation.Load(); got != "owner" {
		t.Errorf("affiliation = %v, want owner", got)
	}
	if len(repos) != 2 {
		t.Fatalf("len = %d, want 2 (is_template filter)", len(repos))
	}
	if repos[0].FullName != "acme/expo-tpl" || repos[0].Description != "Expo starter" {
		t.Errorf("unexpected first repo: %+v", repos[0])
	}
	if repos[1].FullName != "acme/go-tpl" || !repos[1].Private {
		t.Errorf("unexpected second repo (private templates must be included): %+v", repos[1])
	}
}

func TestResolveTemplateRepo(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/expo-tpl":
			_, _ = w.Write([]byte(`{"name":"expo-tpl","full_name":"acme/expo-tpl","is_template":true,
				"clone_url":"https://github.example/acme/expo-tpl.git","owner":{"login":"acme"}}`))
		case "/repos/acme/not-a-template":
			_, _ = w.Write([]byte(`{"name":"not-a-template","full_name":"acme/not-a-template",
				"is_template":false,"clone_url":"https://github.example/acme/not-a-template.git","owner":{"login":"acme"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)
	ctx := context.Background()

	cloneURL, err := m.ResolveTemplateRepo(ctx, "gho_test", "acme", "expo-tpl")
	if err != nil {
		t.Fatalf("resolve template: %v", err)
	}
	if cloneURL != "https://github.example/acme/expo-tpl.git" {
		t.Errorf("cloneURL = %q", cloneURL)
	}

	if _, err := m.ResolveTemplateRepo(ctx, "gho_test", "acme", "not-a-template"); !errors.Is(err, ErrNotTemplate) {
		t.Errorf("non-template: got %v, want ErrNotTemplate", err)
	}

	if _, err := m.ResolveTemplateRepo(ctx, "gho_test", "acme", "ghost"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("missing repo: got %v, want ErrRepoNotFound", err)
	}
}
