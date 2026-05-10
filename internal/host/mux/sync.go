package hostmux

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// handleSyncApply applies a checkpoint into a per-user working tree
// under ~/work/<repo>/. The body is multipart/form-data with three
// fields: a JSON manifest plus two bundles.
//
// Query params:
//
//	repo  — relative path under ~/work/ (e.g. "myproject"). Must be a
//	        single path segment; ".." or absolute paths are rejected to
//	        keep blast radius inside ~/work/.
//
// Multipart fields (FormFile resolves each by name; order doesn't
// matter):
//
//	"manifest"     — application/json: the checkpoint.Manifest JSON.
//	"head_commit"  — application/octet-stream: the headCommit bundle.
//	"incremental"  — application/octet-stream: the incremental bundle.
//
// After Apply, the working tree at ~/work/<repo> matches the manifest
// exactly — HEAD, branch, index, and untracked files all restored.
//
// Returns 204 on success. Errors are JSON {code, error}.
func (m *Mux) handleSyncApply(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if !validRepoSlug(repo) {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_repo", Error: "repo must be a single non-empty path segment without '..' or '/'"})
		return
	}

	workDir, err := workRoot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: "work_root", Error: err.Error()})
		return
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: "mkdir_work", Error: err.Error()})
		return
	}
	target := filepath.Join(workDir, repo)

	// 256 MiB cap — large for typical edits but bounded so a malicious
	// client can't fill the disk via in-memory parts.
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_multipart", Error: err.Error()})
		return
	}

	manifestPart, _, err := r.FormFile("manifest")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "missing_manifest", Error: err.Error()})
		return
	}
	defer manifestPart.Close()
	manifestBytes, err := io.ReadAll(io.LimitReader(manifestPart, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "read_manifest", Error: err.Error()})
		return
	}
	var manifest checkpoint.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "parse_manifest", Error: err.Error()})
		return
	}

	headPart, _, err := r.FormFile("head_commit")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "missing_head_commit", Error: err.Error()})
		return
	}
	defer headPart.Close()

	incrPart, _, err := r.FormFile("incremental")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "missing_incremental", Error: err.Error()})
		return
	}
	defer incrPart.Close()

	if err := checkpoint.Apply(r.Context(), target, &manifest, headPart, incrPart); err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: "apply_checkpoint", Error: err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validRepoSlug(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// workRoot resolves $HOME/work for the sandbox user. clank-host runs as
// the same user that owns the volume, so $HOME is the right anchor.
func workRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, "work"), nil
}
