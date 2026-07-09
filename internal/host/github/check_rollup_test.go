package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	gogithub "github.com/google/go-github/v66/github"
)

func TestCheckRollupForRef_AggregatesAndAuthenticates(t *testing.T) {
	t.Parallel()
	var gotAuth atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/repos/acme/api/commits/abc123/check-runs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":4,"check_runs":[
			{"name":"build","status":"completed","conclusion":"success"},
			{"name":"lint","status":"completed","conclusion":"skipped"},
			{"name":"test","status":"in_progress","conclusion":null},
			{"name":"e2e","status":"queued","conclusion":null}
		]}`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	rollup, err := m.CheckRollupForRef(context.Background(), "gho_test", "acme", "api", "abc123")
	if err != nil {
		t.Fatalf("CheckRollupForRef: %v", err)
	}
	want := CheckRollup{State: CheckStatePending, Passed: 2, Failed: 0, Pending: 2, Total: 4}
	if rollup == nil || *rollup != want {
		t.Errorf("rollup = %+v, want %+v", rollup, want)
	}
	if a, _ := gotAuth.Load().(string); a != "Bearer gho_test" {
		t.Errorf("Authorization = %q, want Bearer gho_test", a)
	}
}

func TestCheckRollupForRef_NoCheckRunsIsNil(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	rollup, err := m.CheckRollupForRef(context.Background(), "gho_test", "acme", "api", "abc123")
	if err != nil {
		t.Fatalf("CheckRollupForRef: %v", err)
	}
	if rollup != nil {
		t.Errorf("rollup = %+v, want nil (no CI configured must not render as green)", rollup)
	}
}

func TestCheckRollupForRef_Paginates(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[
				{"name":"page2","status":"completed","conclusion":"failure"}]}`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, "http://"+r.Host, r.URL.Path))
		_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[
			{"name":"page1","status":"completed","conclusion":"success"}]}`))
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	rollup, err := m.CheckRollupForRef(context.Background(), "gho_test", "acme", "api", "abc123")
	if err != nil {
		t.Fatalf("CheckRollupForRef: %v", err)
	}
	want := CheckRollup{State: CheckStateFailing, Passed: 1, Failed: 1, Total: 2}
	if rollup == nil || *rollup != want {
		t.Errorf("rollup = %+v, want %+v (both pages counted)", rollup, want)
	}
}

// The bucket/verdict table: GitHub's PR-list semantics. neutral and
// skipped pass; every non-passing completed conclusion fails; failing
// dominates pending (red shows as soon as one check fails, even
// mid-run).
func TestRollupCheckRuns(t *testing.T) {
	t.Parallel()
	run := func(status, conclusion string) *gogithub.CheckRun {
		return &gogithub.CheckRun{Status: &status, Conclusion: &conclusion}
	}
	cases := []struct {
		name string
		runs []*gogithub.CheckRun
		want CheckRollup
	}{
		{
			name: "all passing incl neutral and skipped",
			runs: []*gogithub.CheckRun{
				run("completed", "success"), run("completed", "neutral"), run("completed", "skipped"),
			},
			want: CheckRollup{State: CheckStatePassing, Passed: 3, Total: 3},
		},
		{
			name: "every failing conclusion",
			runs: []*gogithub.CheckRun{
				run("completed", "failure"), run("completed", "timed_out"),
				run("completed", "cancelled"), run("completed", "action_required"),
				run("completed", "stale"), run("completed", "startup_failure"),
			},
			want: CheckRollup{State: CheckStateFailing, Failed: 6, Total: 6},
		},
		{
			name: "failing dominates pending",
			runs: []*gogithub.CheckRun{
				run("completed", "failure"), run("in_progress", ""), run("completed", "success"),
			},
			want: CheckRollup{State: CheckStateFailing, Passed: 1, Failed: 1, Pending: 1, Total: 3},
		},
		{
			name: "pending while green so far",
			runs: []*gogithub.CheckRun{
				run("queued", ""), run("completed", "success"),
			},
			want: CheckRollup{State: CheckStatePending, Passed: 1, Pending: 1, Total: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rollupCheckRuns(tc.runs); got != tc.want {
				t.Errorf("rollupCheckRuns = %+v, want %+v", got, tc.want)
			}
		})
	}
}
