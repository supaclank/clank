package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOverlayAPIPathAllowed_ProfileWrites(t *testing.T) {
	t.Parallel()
	if !overlayAPIPathAllowed(http.MethodPost, "/presets") {
		t.Fatal("owner overlay must be allowed to save a custom profile")
	}
	if overlayAPIPathAllowed(http.MethodDelete, "/presets/custom") {
		t.Fatal("profile deletion is outside the overlay save-as-new flow")
	}
}

func TestOverlayAPIPathAllowed_SourceControl(t *testing.T) {
	t.Parallel()
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/credentials/github/status"},
		{http.MethodPost, "/credentials/github/connect/start"},
		{http.MethodGet, "/credentials/github/connect/status"},
		{http.MethodPost, "/credentials/github/connect/cancel"},
		{http.MethodPost, "/worktrees/list-branches"},
		{http.MethodGet, "/worktrees/wt1/remote/status"},
		{http.MethodPost, "/worktrees/wt1/remote/push"},
		{http.MethodPost, "/worktrees/wt1/remote/pull"},
		{http.MethodPost, "/worktrees/wt1/remote/resolve"},
		{http.MethodPost, "/worktrees/wt1/remote/publish"},
		{http.MethodPost, "/worktrees/wt1/pr"},
		{http.MethodPost, "/worktrees/wt1/pr/preview"},
		{http.MethodPost, "/worktrees/wt1/pr/ready"},
	}
	for _, tc := range allowed {
		if !overlayAPIPathAllowed(tc.method, tc.path) {
			t.Errorf("%s %s must be allowed for the source-control overlay", tc.method, tc.path)
		}
	}
	denied := []struct{ method, path string }{
		// Disconnect stays out: destroying the host's GitHub connection
		// is an app/CLI action, not an overlay one.
		{http.MethodDelete, "/credentials/github"},
		{http.MethodGet, "/credentials/github/repos"},
		// Worktree lifecycle + preview control plane stay out.
		{http.MethodPost, "/worktrees/wt1/preview/start"},
		{http.MethodDelete, "/worktrees/wt1"},
		{http.MethodPost, "/worktrees/resolve"},
		{http.MethodPost, "/worktrees/merge"},
		// The GitRef-addressed sync routes are local-preview only; the
		// hosted overlay must use the {id}-keyed ones so path scoping
		// applies (list-branches is the one body-scoped exception).
		{http.MethodPost, "/worktrees/remote-status"},
		{http.MethodPost, "/worktrees/remote-push"},
		{http.MethodPost, "/worktrees/create-pr"},
		{http.MethodGet, "/worktrees/wt1/remote/status/extra"},
	}
	for _, tc := range denied {
		if overlayAPIPathAllowed(tc.method, tc.path) {
			t.Errorf("%s %s must NOT be allowed through the overlay API", tc.method, tc.path)
		}
	}
}

func TestOverlayWorktreeRoute(t *testing.T) {
	t.Parallel()
	if id, suffix, ok := overlayWorktreeRoute("/worktrees/wt%2F1/remote/status"); !ok || id != "wt/1" || suffix != "/remote/status" {
		t.Errorf("escaped id parse = (%q, %q, %v)", id, suffix, ok)
	}
	if _, _, ok := overlayWorktreeRoute("/worktrees/list-branches"); ok {
		t.Error("body-addressed route must not parse as an id route")
	}
	if _, _, ok := overlayWorktreeRoute("/sessions/abc/messages"); ok {
		t.Error("session route must not parse as a worktree route")
	}
}

func TestValidateOverlayWorktreeRef(t *testing.T) {
	t.Parallel()
	newRequest := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/worktrees/list-branches", bytes.NewBufferString(body))
	}

	// Missing git_ref.worktree_id is malformed input, distinct from an
	// actual cross-worktree mismatch — must not collapse into 403.
	w := httptest.NewRecorder()
	if validateOverlayWorktreeRef(w, newRequest(`{}`), "wt1") {
		t.Fatal("empty worktree_id must be rejected")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty worktree_id status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	w = httptest.NewRecorder()
	if validateOverlayWorktreeRef(w, newRequest(`{"git_ref":{"worktree_id":"other"}}`), "wt1") {
		t.Fatal("mismatched worktree_id must be rejected")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("mismatched worktree_id status = %d, want %d", w.Code, http.StatusForbidden)
	}

	w = httptest.NewRecorder()
	if !validateOverlayWorktreeRef(w, newRequest(`{"git_ref":{"worktree_id":"wt1"}}`), "wt1") {
		t.Fatal("matching worktree_id must be accepted")
	}

	// local_path set with no worktree_id is still the hosted-preview policy
	// violation (403), not malformed input (400) — local_path is never valid here.
	w = httptest.NewRecorder()
	if validateOverlayWorktreeRef(w, newRequest(`{"git_ref":{"local_path":"/etc"}}`), "wt1") {
		t.Fatal("local_path without worktree_id must be rejected")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("local_path without worktree_id status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestValidateOverlayWorktreeRef_LocalPathRejected(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	body := `{"git_ref":{"worktree_id":"wt1","local_path":"/tmp/x"}}`
	if validateOverlayWorktreeRef(w, httptest.NewRequest(http.MethodPost, "/worktrees/list-branches", strings.NewReader(body)), "wt1") {
		t.Fatal("body-addressed local_path must be rejected for hosted previews")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}
