package hostmux

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/supaclank/clank/internal/git"
	"github.com/supaclank/clank/internal/host"
)

// TestWriteRemoteError_Mapping locks the error→(status, code) contract the
// mobile client maps back to sentinels. Includes two fall-through cases to
// prove the default writeError path still applies.
func TestWriteRemoteError_Mapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"github_unavailable", host.ErrGitHubManagerUnavailable, http.StatusServiceUnavailable, "github_unavailable"},
		{"not_connected", host.ErrGitHubNotConnected, http.StatusForbidden, "github_not_connected"},
		{"no_origin", host.ErrNoOriginRemote, http.StatusBadRequest, "no_origin_remote"},
		{"detached_head", host.ErrDetachedHead, http.StatusConflict, "detached_head"},
		{"no_upstream", host.ErrNoUpstream, http.StatusBadRequest, "no_upstream"},
		{"worktree_dirty", host.ErrWorktreeDirty, http.StatusConflict, "worktree_dirty"},
		{"remote_diverged", host.ErrRemoteDiverged, http.StatusConflict, "remote_diverged"},
		{"not_merging", host.ErrNotMerging, http.StatusConflict, "not_merging"},
		{"repo_not_accessible", git.ErrPushRepoNotFound, http.StatusForbidden, "github_repo_not_accessible"},
		{"push_denied", git.ErrPushPermissionDenied, http.StatusForbidden, "push_denied"},
		{"invalid_arg_fallthrough", host.ErrInvalidArgument, http.StatusBadRequest, "invalid_argument"},
		{"not_found_fallthrough", host.ErrNotFound, http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			writeRemoteError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var body errResp
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Code, tc.wantCode)
			}
		})
	}
}
