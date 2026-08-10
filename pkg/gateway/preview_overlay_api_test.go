package gateway

import (
	"net/http"
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
