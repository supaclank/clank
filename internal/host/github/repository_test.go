package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetRepositorySupportsAnonymousPublicRepository(t *testing.T) {
	t.Parallel()
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.URL.Path != "/repos/acme/api" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name":"api","full_name":"acme/api","html_url":"https://github.com/acme/api",
			"description":"An API","private":false,"default_branch":"trunk",
			"owner":{"login":"acme"}
		}`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "")
	m.SetAPIBaseURL(srv.URL)
	got, err := m.GetRepository(context.Background(), "", "acme", "api")
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if authorization != "" {
		t.Errorf("Authorization = %q, want anonymous request", authorization)
	}
	if got.Owner != "acme" || got.Name != "api" || got.DefaultBranch != "trunk" || got.Description != "An API" || got.IsPrivate {
		t.Errorf("repository = %+v", got)
	}
}

func TestGetRepositoryClassifiesNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), "")
	m.SetAPIBaseURL(srv.URL)

	_, err := m.GetRepository(context.Background(), "", "acme", "private")
	if !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("error = %v, want ErrRepositoryNotFound", err)
	}
}
