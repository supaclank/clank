package hostmux

import (
	"net/http"

	"github.com/acksell/clank/internal/agent"
)

// Worktree/branch endpoints. The /repos surface was removed in §7.8 of
// hub_host_refactor_code_review.md alongside the host repo registry —
// callers now identify the repo by its GitRef in the request body.

type worktreeBranchRequest struct {
	GitRef agent.GitRef `json:"git_ref"`
	Branch string       `json:"branch"`
	Force  bool         `json:"force,omitempty"`
}

type mergeBranchRequest struct {
	GitRef        agent.GitRef `json:"git_ref"`
	Branch        string       `json:"branch"`
	CommitMessage string       `json:"commit_message,omitempty"`
}

// createWorktreeRequest is the body of POST /worktrees/create. The
// host identifies the target repo via base_worktree_id (any existing
// worktree in the desired repo); host-local paths never appear on the
// wire because they're not comparable across the laptop/sprite
// boundary.
type createWorktreeRequest struct {
	BaseWorktreeID string `json:"base_worktree_id"`
	BaseBranch     string `json:"base_branch"`
}

// HOST
func (m *Mux) handleListBranches(w http.ResponseWriter, r *http.Request) {
	var req worktreeBranchRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := req.GitRef.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	out, err := m.svc.ListBranches(r.Context(), req.GitRef)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// HOST
func (m *Mux) handleResolveWorktree(w http.ResponseWriter, r *http.Request) {
	var req worktreeBranchRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := req.GitRef.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.Branch == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "branch is required"})
		return
	}
	out, err := m.svc.ResolveWorktree(r.Context(), req.GitRef, req.Branch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// HOST
func (m *Mux) handleRemoveWorktree(w http.ResponseWriter, r *http.Request) {
	var req worktreeBranchRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := req.GitRef.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.Branch == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "branch is required"})
		return
	}
	if err := m.svc.RemoveWorktree(r.Context(), req.GitRef, req.Branch, req.Force); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HOST
// handleDeleteMaterializedWorktree services DELETE /worktrees/{id}: the
// full-cleanup leg of a worktree delete. Removes the materialized
// ~/work/{id} directory and every session belonging to the worktree.
// Returns 409 (worktree_busy) when a session is actively running.
func (m *Mux) handleDeleteMaterializedWorktree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "worktree id is required"})
		return
	}
	if err := m.svc.DeleteMaterializedWorktree(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HOST
func (m *Mux) handleCreateWorktree(w http.ResponseWriter, r *http.Request) {
	var req createWorktreeRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.BaseWorktreeID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "base_worktree_id is required"})
		return
	}
	if req.BaseBranch == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "base_branch is required"})
		return
	}
	out, err := m.svc.CreateWorktree(r.Context(), agent.GitRef{WorktreeID: req.BaseWorktreeID}, req.BaseBranch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// HOST
func (m *Mux) handleMergeBranch(w http.ResponseWriter, r *http.Request) {
	var req mergeBranchRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := req.GitRef.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.Branch == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "branch is required"})
		return
	}
	out, err := m.svc.MergeBranch(r.Context(), req.GitRef, req.Branch, req.CommitMessage)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
