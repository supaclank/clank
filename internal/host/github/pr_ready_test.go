package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeReadyAPI is a scripted httptest.Server simulating the two calls
// MarkPRReadyForReview makes: GET /repos/{o}/{r}/pulls/{n} (REST
// detail) and POST /graphql (the mutation).
type fakeReadyAPI struct {
	getStatus int
	getBody   string
	gqlStatus int
	gqlBody   string

	gqlReqs atomic.Int64
	gotGQL  atomic.Value // string: raw graphql request body
	gotAuth atomic.Value // string: Authorization header on /graphql
}

func newFakeReadyAPI(t *testing.T, fa *fakeReadyAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fa.getStatus)
			_, _ = w.Write([]byte(fa.getBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			fa.gqlReqs.Add(1)
			fa.gotAuth.Store(r.Header.Get("Authorization"))
			body, _ := io.ReadAll(r.Body)
			fa.gotGQL.Store(string(body))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fa.gqlStatus)
			_, _ = w.Write([]byte(fa.gqlBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMarkPRReadyForReview_FlipsDraft(t *testing.T) {
	t.Parallel()
	fa := &fakeReadyAPI{
		getStatus: http.StatusOK,
		getBody:   `{"number": 7, "draft": true, "node_id": "PR_kwDOtest7"}`,
		gqlStatus: http.StatusOK,
		gqlBody:   `{"data": {"markPullRequestReadyForReview": {"pullRequest": {"isDraft": false}}}}`,
	}
	srv := newFakeReadyAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	if err := m.MarkPRReadyForReview(context.Background(), "gho_test", "acme", "api", 7); err != nil {
		t.Fatalf("MarkPRReadyForReview: %v", err)
	}
	if fa.gqlReqs.Load() != 1 {
		t.Fatalf("graphql requests = %d, want 1", fa.gqlReqs.Load())
	}
	body, _ := fa.gotGQL.Load().(string)
	if !strings.Contains(body, "markPullRequestReadyForReview") {
		t.Errorf("graphql body missing mutation name: %s", body)
	}
	if !strings.Contains(body, "PR_kwDOtest7") {
		t.Errorf("graphql body missing node id: %s", body)
	}
	if auth, _ := fa.gotAuth.Load().(string); auth != "Bearer gho_test" {
		t.Errorf("graphql Authorization = %q, want Bearer gho_test", auth)
	}
}

// An already-ready PR is a successful no-op — no mutation is sent, so
// a double-tap or a flip raced from the web UI can't error.
func TestMarkPRReadyForReview_AlreadyReady_NoMutation(t *testing.T) {
	t.Parallel()
	fa := &fakeReadyAPI{
		getStatus: http.StatusOK,
		getBody:   `{"number": 7, "draft": false, "node_id": "PR_kwDOtest7"}`,
	}
	srv := newFakeReadyAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	if err := m.MarkPRReadyForReview(context.Background(), "gho_test", "acme", "api", 7); err != nil {
		t.Fatalf("MarkPRReadyForReview: %v", err)
	}
	if fa.gqlReqs.Load() != 0 {
		t.Errorf("graphql requests = %d, want 0 for already-ready PR", fa.gqlReqs.Load())
	}
}

// GraphQL errors arrive with HTTP 200 — the errors array must fail
// the call, not silently pass.
func TestMarkPRReadyForReview_GraphQLError(t *testing.T) {
	t.Parallel()
	fa := &fakeReadyAPI{
		getStatus: http.StatusOK,
		getBody:   `{"number": 7, "draft": true, "node_id": "PR_kwDOtest7"}`,
		gqlStatus: http.StatusOK,
		gqlBody:   `{"data": null, "errors": [{"type": "UNPROCESSABLE", "message": "Pull request is not in a draft state"}]}`,
	}
	srv := newFakeReadyAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	err := m.MarkPRReadyForReview(context.Background(), "gho_test", "acme", "api", 7)
	if err == nil || !strings.Contains(err.Error(), "not in a draft state") {
		t.Fatalf("err = %v, want graphql error message surfaced", err)
	}
}

// The FORBIDDEN error isn't always first in the errors array — the
// classifier must scan all of them, not just body.Errors[0].
func TestMarkPRReadyForReview_GraphQLForbidden_NotFirstError(t *testing.T) {
	t.Parallel()
	fa := &fakeReadyAPI{
		getStatus: http.StatusOK,
		getBody:   `{"number": 7, "draft": true, "node_id": "PR_kwDOtest7"}`,
		gqlStatus: http.StatusOK,
		gqlBody:   `{"data": null, "errors": [{"type": "SOME_OTHER", "message": "unrelated"}, {"type": "FORBIDDEN", "message": "Resource not accessible"}]}`,
	}
	srv := newFakeReadyAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	err := m.MarkPRReadyForReview(context.Background(), "gho_test", "acme", "api", 7)
	if !errors.Is(err, ErrPRForbidden) {
		t.Fatalf("err = %v, want ErrPRForbidden", err)
	}
}

func TestMarkPRReadyForReview_GraphQLForbidden(t *testing.T) {
	t.Parallel()
	fa := &fakeReadyAPI{
		getStatus: http.StatusOK,
		getBody:   `{"number": 7, "draft": true, "node_id": "PR_kwDOtest7"}`,
		gqlStatus: http.StatusOK,
		gqlBody:   `{"data": null, "errors": [{"type": "FORBIDDEN", "message": "Resource not accessible"}]}`,
	}
	srv := newFakeReadyAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	err := m.MarkPRReadyForReview(context.Background(), "gho_test", "acme", "api", 7)
	if !errors.Is(err, ErrPRForbidden) {
		t.Fatalf("err = %v, want ErrPRForbidden", err)
	}
}

func TestMarkPRReadyForReview_TokenInvalid(t *testing.T) {
	t.Parallel()
	fa := &fakeReadyAPI{
		getStatus: http.StatusUnauthorized,
		getBody:   `{"message": "Bad credentials"}`,
	}
	srv := newFakeReadyAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	err := m.MarkPRReadyForReview(context.Background(), "gho_bad", "acme", "api", 7)
	if !errors.Is(err, ErrPRTokenInvalid) {
		t.Fatalf("err = %v, want ErrPRTokenInvalid", err)
	}
}

func TestMarkPRReadyForReview_NotFound_MapsToForbidden(t *testing.T) {
	t.Parallel()
	fa := &fakeReadyAPI{
		getStatus: http.StatusNotFound,
		getBody:   `{"message": "Not Found"}`,
	}
	srv := newFakeReadyAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	err := m.MarkPRReadyForReview(context.Background(), "gho_test", "acme", "api", 404)
	if !errors.Is(err, ErrPRForbidden) {
		t.Fatalf("err = %v, want ErrPRForbidden", err)
	}
}
