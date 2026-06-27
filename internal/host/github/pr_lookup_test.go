package github

import (
	"context"
	"net/http"
	"testing"
)

func TestFindOpenPRForBranch_Found(t *testing.T) {
	t.Parallel()
	fa := &fakeAPI{
		getStatus: http.StatusOK,
		getBody:   `[{"number": 7, "html_url": "https://github.com/acme/api/pull/7", "head": {"sha": "abc"}}]`,
	}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	pr, err := m.FindOpenPRForBranch(context.Background(), "gho_test", "acme", "api", "my-branch")
	if err != nil {
		t.Fatalf("FindOpenPRForBranch: %v", err)
	}
	if pr == nil {
		t.Fatal("expected a PR, got nil")
	}
	if pr.Number != 7 {
		t.Errorf("Number = %d, want 7", pr.Number)
	}
	if pr.HTMLURL != "https://github.com/acme/api/pull/7" {
		t.Errorf("HTMLURL = %q", pr.HTMLURL)
	}
}

// TestFindOpenPRForBranch_None: an empty array is "no open PR", not an
// error — the caller renders the no-PR state (offer to create one).
func TestFindOpenPRForBranch_None(t *testing.T) {
	t.Parallel()
	fa := &fakeAPI{getStatus: http.StatusOK, getBody: `[]`}
	srv := newFakeAPI(t, fa)
	m := newPRTestManager(t, srv.URL)

	pr, err := m.FindOpenPRForBranch(context.Background(), "gho_test", "acme", "api", "missing")
	if err != nil {
		t.Fatalf("FindOpenPRForBranch: %v", err)
	}
	if pr != nil {
		t.Errorf("expected nil PR, got %+v", pr)
	}
}
