package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAPI is a scripted httptest.Server simulating api.github.com.
// Each test supplies the response shape for the POST /pulls endpoint
// and (optionally) the GET /pulls follow-up.
type fakeAPI struct {
	postReqs atomic.Int64
	getReqs  atomic.Int64

	postStatus   int
	postBody     string
	getStatus    int
	getBody      string
	gotAuthorize string
}

func newFakeAPI(t *testing.T, fa *fakeAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fa.gotAuthorize = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			fa.postReqs.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fa.postStatus)
			_, _ = w.Write([]byte(fa.postBody))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			fa.getReqs.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fa.getStatus)
			_, _ = w.Write([]byte(fa.getBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newPRTestManager(t *testing.T, apiURL string) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(apiURL)
	return m
}

func TestCreatePullRequest_Success(t *testing.T) {
	t.Parallel()
	fa := &fakeAPI{
		postStatus: http.StatusCreated,
		postBody: `{
			"number": 42,
			"html_url": "https://github.com/acme/api/pull/42",
			"head": {"sha": "abc123"}
		}`,
	}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	pr, err := m.CreatePullRequest(context.Background(), "gho_test", "acme", "api", CreatePRInput{
		Title: "feat: add x",
		Body:  "this adds x",
		Head:  "my-branch",
		Base:  "main",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.HTMLURL != "https://github.com/acme/api/pull/42" {
		t.Errorf("HTMLURL = %q", pr.HTMLURL)
	}
	if pr.Head.SHA != "abc123" {
		t.Errorf("Head.SHA = %q", pr.Head.SHA)
	}
	if fa.gotAuthorize != "Bearer gho_test" {
		t.Errorf("Authorization header = %q", fa.gotAuthorize)
	}
}

func TestCreatePullRequest_AlreadyExists(t *testing.T) {
	t.Parallel()
	fa := &fakeAPI{
		postStatus: http.StatusUnprocessableEntity,
		postBody: `{
			"message": "Validation Failed",
			"errors": [{"message": "A pull request already exists for acme:my-branch."}]
		}`,
		getStatus: http.StatusOK,
		getBody:   `[{"number": 7, "html_url": "https://github.com/acme/api/pull/7", "head": {"sha": "old"}}]`,
	}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	_, err := m.CreatePullRequest(context.Background(), "gho_test", "acme", "api", CreatePRInput{
		Title: "x", Body: "y", Head: "my-branch", Base: "main",
	})
	if !errors.Is(err, ErrPRAlreadyExists) {
		t.Fatalf("err = %v, want ErrPRAlreadyExists", err)
	}
	if url := ExistingURLFromError(err); url != "https://github.com/acme/api/pull/7" {
		t.Errorf("existing URL = %q, want pull/7", url)
	}
	// The follow-up GET should have fired.
	if fa.getReqs.Load() != 1 {
		t.Errorf("getReqs = %d, want 1", fa.getReqs.Load())
	}
}

func TestCreatePullRequest_BaseNotFound(t *testing.T) {
	t.Parallel()
	fa := &fakeAPI{
		postStatus: http.StatusUnprocessableEntity,
		postBody: `{
			"message": "Validation Failed",
			"errors": [{"message": "Field \"base\" is invalid"}]
		}`,
	}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	_, err := m.CreatePullRequest(context.Background(), "gho_test", "acme", "api", CreatePRInput{
		Title: "x", Body: "y", Head: "my-branch", Base: "nonexistent",
	})
	if !errors.Is(err, ErrPRBaseNotFound) {
		t.Fatalf("err = %v, want ErrPRBaseNotFound", err)
	}
}

func TestCreatePullRequest_Unauthorized(t *testing.T) {
	t.Parallel()
	fa := &fakeAPI{
		postStatus: http.StatusUnauthorized,
		postBody:   `{"message": "Bad credentials"}`,
	}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	_, err := m.CreatePullRequest(context.Background(), "gho_bad", "acme", "api", CreatePRInput{
		Title: "x", Body: "y", Head: "b", Base: "main",
	})
	if !errors.Is(err, ErrPRTokenInvalid) {
		t.Fatalf("err = %v, want ErrPRTokenInvalid", err)
	}
}

func TestCreatePullRequest_Forbidden(t *testing.T) {
	t.Parallel()
	fa := &fakeAPI{
		postStatus: http.StatusForbidden,
		postBody:   `{"message": "Resource not accessible"}`,
	}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	_, err := m.CreatePullRequest(context.Background(), "gho_test", "acme", "api", CreatePRInput{
		Title: "x", Body: "y", Head: "b", Base: "main",
	})
	if !errors.Is(err, ErrPRForbidden) {
		t.Fatalf("err = %v, want ErrPRForbidden", err)
	}
}

func TestCreatePullRequest_RepoNotFound_MapsToForbidden(t *testing.T) {
	// GitHub returns 404 to obscure "no access vs doesn't exist" —
	// we treat both as "forbidden" since we can't tell them apart
	// and the UX is identical.
	t.Parallel()
	fa := &fakeAPI{
		postStatus: http.StatusNotFound,
		postBody:   `{"message": "Not Found"}`,
	}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	_, err := m.CreatePullRequest(context.Background(), "gho_test", "acme", "api", CreatePRInput{
		Title: "x", Body: "y", Head: "b", Base: "main",
	})
	if !errors.Is(err, ErrPRForbidden) {
		t.Fatalf("err = %v, want ErrPRForbidden (got: %v)", err, err)
	}
}
