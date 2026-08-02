package hostmux

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

func TestGitHubPullRequestRoutesAreRegistered(t *testing.T) {
	t.Parallel()
	svc := host.New(host.Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)
	handler := New(svc, nil).Handler()
	for _, path := range []string{
		"/github/pull-requests/inspect",
		"/github/pull-requests/launch",
		"/github/repositories/inspect",
		"/github/repositories/launch",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s status = %d, want registered handler's 400", path, rec.Code)
		}
	}
}

func TestWriteGitHubPullRequestError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "connection required", err: host.ErrGitHubConnectionRequired, wantStatus: http.StatusForbidden, wantCode: "github_connection_required"},
		{name: "head changed", err: host.ErrPullRequestChanged, wantStatus: http.StatusConflict, wantCode: "pull_request_changed"},
		{name: "dirty checkout", err: host.ErrWorktreeDirty, wantStatus: http.StatusConflict, wantCode: "worktree_dirty"},
		{name: "diverged checkout", err: host.ErrRemoteDiverged, wantStatus: http.StatusConflict, wantCode: "remote_diverged"},
		{name: "local commits", err: host.ErrPullRequestLocalCommits, wantStatus: http.StatusConflict, wantCode: "pull_request_local_commits"},
		{name: "ambiguous checkout", err: host.ErrPullRequestRepoAmbiguous, wantStatus: http.StatusConflict, wantCode: "pull_request_repo_ambiguous"},
		{name: "invalid", err: host.ErrInvalidArgument, wantStatus: http.StatusBadRequest, wantCode: "invalid_argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			writeGitHubPullRequestError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var body errResp
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Code, tc.wantCode)
			}
		})
	}
}

func TestWriteGitHubRepositoryError(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeGitHubRepositoryError(rec, host.ErrGitHubRepositoryConnectionRequired)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	var body errResp
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "github_connection_required" {
		t.Errorf("code = %q, want github_connection_required", body.Code)
	}
}
