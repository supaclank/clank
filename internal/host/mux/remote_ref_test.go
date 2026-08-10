package hostmux

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
)

// The ref-addressed routes exist for callers without a managed worktree
// id (the local web-preview overlay). Registration is pinned the same
// way the pull-request routes are: an empty body must reach the
// handler's own 400, not the mux's 404.
func TestRemoteRefRoutesAreRegistered(t *testing.T) {
	t.Parallel()
	svc := host.New(host.Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)
	handler := New(svc, nil).Handler()
	for _, path := range []string{
		"/worktrees/remote-status",
		"/worktrees/remote-push",
		"/worktrees/remote-pull",
		"/worktrees/remote-resolve",
		"/worktrees/remote-publish",
		"/worktrees/create-pr",
		"/worktrees/pr-ready",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s status = %d, want registered handler's 400", path, rec.Code)
		}
	}
}

// An empty git_ref must be rejected at decode time with a machine code,
// before any service work happens.
func TestRemoteRefRoutesRejectEmptyGitRef(t *testing.T) {
	t.Parallel()
	svc := host.New(host.Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)
	handler := New(svc, nil).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/worktrees/remote-status", bytes.NewBufferString(`{"git_ref":{}}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body errResp
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", body.Code)
	}
}

// The resolve route validates strategy after the ref, mirroring the
// {id}-keyed handler's contract.
func TestRemoteResolveRefRequiresStrategy(t *testing.T) {
	t.Parallel()
	svc := host.New(host.Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)
	handler := New(svc, nil).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/worktrees/remote-resolve",
		bytes.NewBufferString(`{"git_ref":{"worktree_id":"01TESTWORKTREE0000000000"}}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body errResp
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "missing_field" {
		t.Errorf("code = %q, want missing_field", body.Code)
	}
}

// decodeRefBody must reject an oversized body with 413 before decoding
// it, not treat it as malformed JSON — these routes serve unauthenticated
// browser callers, so an unbounded read is a resource-exhaustion vector.
func TestRemoteRefRoutesRejectOversizedBody(t *testing.T) {
	t.Parallel()
	svc := host.New(host.Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)
	handler := New(svc, nil).Handler()

	oversized := `{"git_ref":{"worktree_id":"01TESTWORKTREE0000000000"},"padding":"` +
		strings.Repeat("x", maxRefBody) + `"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/worktrees/remote-status", bytes.NewBufferString(oversized))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	var body errResp
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "request_too_large" {
		t.Errorf("code = %q, want request_too_large", body.Code)
	}
}

// create-pr promotes the embedded CreatePRRequest fields to the top
// level of the body — pin that wire shape so a client sending
// {git_ref, title, base} round-trips into the service's validation
// (github manager is absent here, so a fully-formed request reaches
// the 503 github_unavailable mapping, proving the fields decoded).
func TestCreatePRRefBodyShape(t *testing.T) {
	t.Parallel()
	svc := host.New(host.Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)
	handler := New(svc, nil).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/worktrees/create-pr",
		bytes.NewBufferString(`{"git_ref":{"worktree_id":"01TESTWORKTREE0000000000"},"title":"t","body":"b","base":"main"}`))
	handler.ServeHTTP(rec, req)
	var body errResp
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code == "pr_missing_field" {
		t.Fatalf("title/base did not decode from the top level: %+v", body)
	}
}
