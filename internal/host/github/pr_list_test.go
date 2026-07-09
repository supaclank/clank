package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestListPullRequests_TrimsAndAuthenticates(t *testing.T) {
	t.Parallel()
	var gotAuth, gotState atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotState.Store(r.URL.Query().Get("state"))
		if r.URL.Path != "/repos/acme/api/pulls" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":7,"title":"Add dark mode","state":"open","draft":false,
			 "html_url":"https://github.com/acme/api/pull/7",
			 "head":{"ref":"dark-mode","sha":"abc"},"base":{"ref":"main"},
			 "user":{"login":"octocat"},"updated_at":"2026-01-02T03:04:05Z"},
			{"number":8,"title":"WIP","state":"open","draft":true,
			 "html_url":"https://github.com/acme/api/pull/8",
			 "head":{"ref":"wip"},"base":{"ref":"main"},
			 "user":{"login":"hubot"},"updated_at":"2026-01-01T00:00:00Z"}
		]`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	pulls, err := m.ListPullRequests(context.Background(), "gho_test", "acme", "api", PRListStateOpen)
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if s, _ := gotState.Load().(string); s != "open" {
		t.Errorf("state param = %q, want open", s)
	}
	if len(pulls) != 2 {
		t.Fatalf("len(pulls) = %d, want 2", len(pulls))
	}
	got := pulls[0]
	if got.Number != 7 || got.Title != "Add dark mode" || got.State != "open" ||
		got.Draft || got.HeadBranch != "dark-mode" || got.BaseBranch != "main" ||
		got.Author != "octocat" || got.HTMLURL != "https://github.com/acme/api/pull/7" {
		t.Errorf("pulls[0] = %+v, unexpected fields", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("pulls[0].UpdatedAt is zero, want parsed timestamp")
	}
	if !pulls[1].Draft {
		t.Error("pulls[1].Draft = false, want true")
	}
	if a, _ := gotAuth.Load().(string); a != "Bearer gho_test" {
		t.Errorf("Authorization = %q, want Bearer gho_test", a)
	}
}

// Closed listings pass state=closed through and surface merged_at — the
// merged-vs-abandoned discriminator (GitHub reports both as "closed").
func TestListPullRequests_ClosedStateCarriesMergedAt(t *testing.T) {
	t.Parallel()
	var gotState atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotState.Store(r.URL.Query().Get("state"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":9,"title":"Shipped","state":"closed","draft":false,
			 "html_url":"https://github.com/acme/api/pull/9",
			 "head":{"ref":"shipped"},"base":{"ref":"main"},
			 "user":{"login":"octocat"},"updated_at":"2026-01-03T00:00:00Z",
			 "merged_at":"2026-01-03T00:00:00Z"},
			{"number":10,"title":"Abandoned","state":"closed","draft":false,
			 "html_url":"https://github.com/acme/api/pull/10",
			 "head":{"ref":"abandoned"},"base":{"ref":"main"},
			 "user":{"login":"octocat"},"updated_at":"2026-01-02T00:00:00Z"}
		]`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	pulls, err := m.ListPullRequests(context.Background(), "gho_test", "acme", "api", PRListStateClosed)
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if s, _ := gotState.Load().(string); s != "closed" {
		t.Errorf("state param = %q, want closed", s)
	}
	if len(pulls) != 2 {
		t.Fatalf("len(pulls) = %d, want 2", len(pulls))
	}
	if pulls[0].MergedAt.IsZero() {
		t.Error("merged PR has zero MergedAt, want parsed timestamp")
	}
	if !pulls[1].MergedAt.IsZero() {
		t.Errorf("abandoned PR MergedAt = %v, want zero", pulls[1].MergedAt)
	}
}
