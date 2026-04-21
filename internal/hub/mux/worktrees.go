package hubmux

import (
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// writeRepoErr maps host-package sentinels (NotFound, merge errors)
// to HTTP statuses, then falls through to writeServiceErr for the
// hub-package sentinels.
func writeRepoErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, host.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, host.ErrCannotMergeDefault),
		errors.Is(err, host.ErrNothingToMerge),
		errors.Is(err, host.ErrCommitMessageRequired),
		errors.Is(err, host.ErrTargetDirty),
		errors.Is(err, host.ErrMergeConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeServiceErr(w, err)
	}
}

func parseHostname(r *http.Request) (host.Hostname, error) {
	id := host.Hostname(r.PathValue("hostname"))
	if id == "" {
		return "", errors.New("hostname is required")
	}
	return id, nil
}

// worktreeRequest is the common body shape for /hosts/{h}/worktrees/*.
// Branch is empty on list-branches; required on resolve/remove/merge.
type worktreeRequest struct {
	GitRef        agent.GitRef `json:"git_ref"`
	Branch        string       `json:"branch,omitempty"`
	Force         bool         `json:"force,omitempty"`
	CommitMessage string       `json:"commit_message,omitempty"`
}

func (m *Mux) handleListBranchesOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	var body worktreeRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON: "+err.Error())
		return
	}
	if err := body.GitRef.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	branches, err := m.svc.ListBranchesOnHost(r.Context(), hostname, body.GitRef)
	if err != nil {
		writeRepoErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, branches)
}

func (m *Mux) handleResolveWorktreeOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	var body worktreeRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON: "+err.Error())
		return
	}
	if err := body.GitRef.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	if body.Branch == "" {
		writeBadRequest(w, "branch is required")
		return
	}
	wt, err := m.svc.ResolveWorktreeOnHost(r.Context(), hostname, body.GitRef, body.Branch)
	if err != nil {
		writeRepoErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wt)
}

func (m *Mux) handleRemoveWorktreeOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	var body worktreeRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON: "+err.Error())
		return
	}
	if err := body.GitRef.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	if body.Branch == "" {
		writeBadRequest(w, "branch is required")
		return
	}
	if err := m.svc.RemoveWorktreeOnHost(r.Context(), hostname, body.GitRef, body.Branch, body.Force); err != nil {
		writeRepoErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mux) handleMergeBranchOnHost(w http.ResponseWriter, r *http.Request) {
	hostname, err := parseHostname(r)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	var body worktreeRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON: "+err.Error())
		return
	}
	if err := body.GitRef.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	if body.Branch == "" {
		writeBadRequest(w, "branch is required")
		return
	}
	res, err := m.svc.MergeBranchOnHost(r.Context(), hostname, body.GitRef, body.Branch, body.CommitMessage)
	if err != nil {
		writeRepoErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
