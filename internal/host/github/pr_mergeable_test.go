package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The tri-state contract: true → mergeable, false → conflicting, and
// null (GitHub still computing the test merge) → unknown without error.
func TestPRMergeable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want MergeableState
	}{
		{name: "clean", body: `{"number":7,"mergeable":true}`, want: MergeableStateMergeable},
		{name: "conflicting", body: `{"number":7,"mergeable":false}`, want: MergeableStateConflicting},
		{name: "still computing", body: `{"number":7,"mergeable":null}`, want: MergeableStateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotAuth atomic.Value // string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth.Store(r.Header.Get("Authorization"))
				if r.URL.Path != "/repos/acme/api/pulls/7" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
			m.SetAPIBaseURL(srv.URL)

			got, err := m.PRMergeable(context.Background(), "gho_test", "acme", "api", 7)
			if err != nil {
				t.Fatalf("PRMergeable: %v", err)
			}
			if got != tc.want {
				t.Errorf("PRMergeable = %q, want %q", got, tc.want)
			}
			if a, _ := gotAuth.Load().(string); a != "Bearer gho_test" {
				t.Errorf("Authorization = %q, want Bearer gho_test", a)
			}
		})
	}
}

func TestPRMergeable_APIError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAPIBaseURL(srv.URL)

	got, err := m.PRMergeable(context.Background(), "gho_test", "acme", "api", 7)
	if err == nil {
		t.Fatal("PRMergeable: err = nil, want error")
	}
	if got != MergeableStateUnknown {
		t.Errorf("PRMergeable = %q on error, want unknown", got)
	}
}
