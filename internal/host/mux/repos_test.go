package hostmux

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acksell/clank/internal/host"
)

// TestWriteRepoError_Mapping locks the repo-scoped error→(status, code)
// contract, with fall-through cases proving the shared writeError path
// still applies for common sentinels.
func TestWriteRepoError_Mapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"repo_not_found", host.ErrRepoNotFound, http.StatusNotFound, "repo_not_found"},
		{"github_unavailable", host.ErrGitHubManagerUnavailable, http.StatusServiceUnavailable, "github_unavailable"},
		{"not_connected", host.ErrGitHubNotConnected, http.StatusForbidden, "github_not_connected"},
		{"branch_not_found_fallthrough", host.ErrNotFound, http.StatusNotFound, "not_found"},
		{"invalid_arg_fallthrough", host.ErrInvalidArgument, http.StatusBadRequest, "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			writeRepoError(rec, tc.err)
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
