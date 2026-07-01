package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gogithub "github.com/google/go-github/v66/github"
)

func TestCreateRepository_CreatesPrivate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"my-app","full_name":"octocat/my-app","private":true,
			"html_url":"https://github.com/octocat/my-app",
			"clone_url":"https://github.com/octocat/my-app.git",
			"owner":{"login":"octocat"}}`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	repo, err := m.CreateRepository(context.Background(), "gho_test", CreateRepoInput{Name: "my-app", Private: true})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	want := CreatedRepo{
		Owner:    "octocat",
		Name:     "my-app",
		FullName: "octocat/my-app",
		Private:  true,
		HTMLURL:  "https://github.com/octocat/my-app",
		CloneURL: "https://github.com/octocat/my-app.git",
	}
	if repo != want {
		t.Errorf("repo = %+v, want %+v", repo, want)
	}
}

func TestCreateRepository_NameTaken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Repository creation failed.",
			"errors":[{"resource":"Repository","field":"name","code":"custom",
			"message":"name already exists on this account"}]}`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	_, err := m.CreateRepository(context.Background(), "gho_test", CreateRepoInput{Name: "taken", Private: true})
	if !errors.Is(err, ErrRepoNameTaken) {
		t.Fatalf("CreateRepository err = %v, want ErrRepoNameTaken", err)
	}
}

func TestClassifyCreateRepoError_NilResponse(t *testing.T) {
	t.Parallel()
	// A nil Response (e.g. a hand-built or mocked error) must not panic on
	// er.Response.StatusCode.
	err := classifyCreateRepoError(&gogithub.ErrorResponse{
		Errors: []gogithub.Error{{Field: "name"}},
	})
	if errors.Is(err, ErrRepoNameTaken) {
		t.Errorf("classifyCreateRepoError with nil Response = %v, want a wrapped generic error, not ErrRepoNameTaken", err)
	}
}
