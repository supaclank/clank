package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetPullRequestSupportsAnonymousPublicRepository(t *testing.T) {
	t.Parallel()
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.URL.Path != "/repos/acme/api/pulls/7" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number":7,"title":"Ship it","html_url":"https://github.com/acme/api/pull/7",
			"head":{"ref":"feature","sha":"0123456789abcdef0123456789abcdef01234567"},
			"base":{"ref":"main","repo":{"private":false}},"user":{"login":"octocat"}
		}`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "")
	m.SetAPIBaseURL(srv.URL)
	got, err := m.GetPullRequest(context.Background(), "", "acme", "api", 7)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if authorization != "" {
		t.Errorf("Authorization = %q, want anonymous request", authorization)
	}
	if got.Number != 7 || got.Author != "octocat" || got.BaseBranch != "main" || got.HeadBranch != "feature" || got.IsPrivate {
		t.Errorf("inspection = %+v", got)
	}
}

func TestGetPullRequestClassifiesNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), "")
	m.SetAPIBaseURL(srv.URL)

	_, err := m.GetPullRequest(context.Background(), "", "acme", "private", 7)
	if !errors.Is(err, ErrPullRequestNotFound) {
		t.Fatalf("error = %v, want ErrPullRequestNotFound", err)
	}
}

func TestGetPullRequestAuthenticatesPrivateRepository(t *testing.T) {
	t.Parallel()
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number":7,"head":{"ref":"feature","sha":"0123456789abcdef0123456789abcdef01234567"},
			"base":{"ref":"main","repo":{"private":true}},"user":{"login":"octocat"}
		}`))
	}))
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), "")
	m.SetAPIBaseURL(srv.URL)

	got, err := m.GetPullRequest(context.Background(), "gho_private", "acme", "private", 7)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer gho_private" {
		t.Errorf("Authorization = %q", authorization)
	}
	if !got.IsPrivate {
		t.Error("IsPrivate = false, want true")
	}
}

func TestOptionalAccessToken(t *testing.T) {
	t.Parallel()
	m := NewManager(t.TempDir(), "")
	token, isConnected, err := m.OptionalAccessToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != "" || isConnected {
		t.Fatalf("token = %q, connected = %t, want anonymous", token, isConnected)
	}
	if err := m.Store().Write(Credentials{AccessToken: "gho_test"}); err != nil {
		t.Fatal(err)
	}
	token, isConnected, err = m.OptionalAccessToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != "gho_test" || !isConnected {
		t.Fatalf("token = %q, connected = %t, want stored token", token, isConnected)
	}
}
