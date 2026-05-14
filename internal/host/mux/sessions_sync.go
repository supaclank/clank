package hostmux

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// session-sync error codes (in addition to the syncErr* set in sync.go).
const (
	syncErrSessionImport = "session_import_failed"
	syncErrSessionExport = "session_export_failed"
)

// sessionBuildsExpiry bounds how long an unconsumed session-build sits
// on disk. Mirror the code bundle expiry.
const sessionBuildsExpiry = 30 * time.Minute

// spriteSessionBuild holds the temp blob files + manifest entries from
// a Service.ExportSessions call, awaiting an upload step that supplies
// the gateway-assigned checkpoint_id and presigned PUT URLs.
type spriteSessionBuild struct {
	result    *host.SessionExportResult
	createdAt time.Time
}

// sessionBuildResponse is the body of POST /sync/sessions/build. The
// gateway uses Entries to mint presigned PUT URLs (one per session
// blob + one for session-manifest.json) and Skipped to surface
// non-opencode sessions to the user.
type sessionBuildResponse struct {
	BuildID string                      `json:"build_id"`
	Entries []checkpoint.SessionEntry   `json:"entries"`
	Skipped []host.SkippedSession       `json:"skipped"`
}

type sessionBuildRequest struct {
	WorktreeID   string `json:"worktree_id"`
	CheckpointID string `json:"checkpoint_id"`
}

// handleSyncSessionsBuild quiesces and exports all sessions for the
// given worktree, returning the manifest entries + a build_id the
// caller uses to drive the upload step. Blobs are held in temp
// files in the build store until upload or expiry.
func (m *Mux) handleSyncSessionsBuild(w http.ResponseWriter, r *http.Request) {
	var req sessionBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "decode body: " + err.Error()})
		return
	}
	if req.WorktreeID == "" || req.CheckpointID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "worktree_id and checkpoint_id are required"})
		return
	}

	result, err := m.svc.ExportSessions(r.Context(), req.WorktreeID, req.CheckpointID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrSessionExport, Error: err.Error()})
		return
	}

	buildID := ulid.Make().String()
	m.sessionBuilds.add(buildID, &spriteSessionBuild{result: result, createdAt: time.Now()})

	writeJSON(w, http.StatusOK, sessionBuildResponse{
		BuildID: buildID,
		Entries: result.Entries,
		Skipped: result.Skipped,
	})
}

// sessionsUploadRequest carries the per-blob presigned PUT URLs the
// gateway minted after calling sync.PresignSessionPuts. SessionURLs
// keys are SessionEntry.SessionID values.
type sessionsUploadRequest struct {
	CheckpointID    string            `json:"checkpoint_id"`
	SessionURLs     map[string]string `json:"session_urls"`
	ManifestPutURL  string            `json:"session_manifest_put_url"`
}

// handleSyncSessionsBuildsUpload finalizes a session-build by
// uploading each per-session blob plus the session-manifest.json to
// the supplied presigned URLs. On success the build is removed; on
// failure the temp files stay so the gateway can retry with fresh URLs.
func (m *Mux) handleSyncSessionsBuildsUpload(w http.ResponseWriter, r *http.Request) {
	buildID := r.PathValue("id")
	if buildID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "build id missing"})
		return
	}
	var req sessionsUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "decode body: " + err.Error()})
		return
	}
	if req.CheckpointID == "" || req.ManifestPutURL == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "checkpoint_id and session_manifest_put_url are required"})
		return
	}

	b := m.sessionBuilds.get(buildID)
	if b == nil {
		writeJSON(w, http.StatusNotFound, errResp{Code: syncErrUnknownBuild, Error: "session build " + buildID + " not found (expired or already uploaded)"})
		return
	}

	cli := &http.Client{Timeout: syncURLBundleFetchTimeout}

	// Upload per-session blobs.
	for _, entry := range b.result.Entries {
		putURL, ok := req.SessionURLs[entry.SessionID]
		if !ok {
			writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "missing presigned URL for session " + entry.SessionID})
			return
		}
		blobPath, ok := b.result.BlobPaths[entry.SessionID]
		if !ok {
			writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrSessionExport, Error: "build missing blob path for session " + entry.SessionID})
			return
		}
		if status, code, err := uploadFile(r.Context(), cli, putURL, blobPath); err != nil {
			writeJSON(w, status, errResp{Code: code, Error: "upload session " + entry.SessionID + ": " + err.Error()})
			return
		}
	}

	// Build + upload the session manifest. CheckpointID is stamped
	// from the gateway's authoritative value.
	manifest := checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: req.CheckpointID,
		Sessions:     b.result.Entries,
		CreatedAt:    time.Now().UTC(),
		CreatedBy:    "host:" + m.svc.ID(),
	}
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrSessionExport, Error: "marshal session manifest: " + err.Error()})
		return
	}
	if status, code, err := uploadBytes(r.Context(), cli, req.ManifestPutURL, manifestBytes, "application/json"); err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "upload session manifest: " + err.Error()})
		return
	}

	m.sessionBuilds.remove(buildID)
	w.WriteHeader(http.StatusNoContent)
}

// handleSyncSessionsBuildsDelete removes a session-build's on-disk
// blobs. Idempotent.
func (m *Mux) handleSyncSessionsBuildsDelete(w http.ResponseWriter, r *http.Request) {
	buildID := r.PathValue("id")
	if buildID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "build id missing"})
		return
	}
	m.sessionBuilds.remove(buildID)
	w.WriteHeader(http.StatusNoContent)
}

// applySessionsRequest is the JSON body of POST
// /sync/sessions/apply-from-urls. SessionBlobURLs keys are
// SessionEntry.SessionID values.
type applySessionsRequest struct {
	WorktreeID      string            `json:"worktree_id"`
	ManifestURL     string            `json:"session_manifest_url"`
	SessionBlobURLs map[string]string `json:"session_blob_urls"`
}

// handleSyncSessionsApplyFromURLs fetches the session manifest +
// each per-session blob from object storage, then invokes
// Service.RegisterImportedSession to install each session into this
// host's host.db and opencode storage.
//
// Returns 204 on success. Partial failures return the first error
// — subsequent sessions are not processed. Re-running is safe
// (RegisterImportedSession is idempotent, opencode import is
// additive-merge keyed on message ID, see plan §E).
func (m *Mux) handleSyncSessionsApplyFromURLs(w http.ResponseWriter, r *http.Request) {
	var req applySessionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "decode body: " + err.Error()})
		return
	}
	if req.WorktreeID == "" || req.ManifestURL == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "worktree_id and session_manifest_url are required"})
		return
	}

	cli := &http.Client{Timeout: syncURLBundleFetchTimeout}

	manifestBytes, status, code, err := fetchURL(r.Context(), cli, req.ManifestURL)
	if err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "fetch session manifest: " + err.Error()})
		return
	}
	if len(manifestBytes) > 1<<20 {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadManifest, Error: "session manifest exceeds 1MiB"})
		return
	}
	manifest, err := checkpoint.UnmarshalSessionManifest(manifestBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadManifest, Error: err.Error()})
		return
	}

	tmpDir, err := os.MkdirTemp("", "clank-session-import-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrSessionImport, Error: "mkdir tempdir: " + err.Error()})
		return
	}
	defer os.RemoveAll(tmpDir)

	for _, entry := range manifest.Sessions {
		getURL, ok := req.SessionBlobURLs[entry.SessionID]
		if !ok {
			writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "missing presigned GET URL for session " + entry.SessionID})
			return
		}
		blobBytes, status, code, err := fetchURL(r.Context(), cli, getURL)
		if err != nil {
			writeJSON(w, status, errResp{Code: code, Error: fmt.Sprintf("fetch session %s blob: %v", entry.SessionID, err)})
			return
		}
		blobPath := filepath.Join(tmpDir, entry.SessionID+".json")
		if err := os.WriteFile(blobPath, blobBytes, 0o600); err != nil {
			writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrSessionImport, Error: "write blob: " + err.Error()})
			return
		}
		if _, err := m.svc.RegisterImportedSession(r.Context(), req.WorktreeID, entry, blobPath); err != nil {
			writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrSessionImport, Error: fmt.Sprintf("register session %s: %v", entry.SessionID, err)})
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// spriteSessionBuildStore mirrors spriteBuildStore for session
// exports. Separate map so build_id collisions between code and
// session builds are impossible.
type spriteSessionBuildStore struct {
	mu     sync.Mutex
	builds map[string]*spriteSessionBuild
}

func newSpriteSessionBuildStore() *spriteSessionBuildStore {
	s := &spriteSessionBuildStore{builds: map[string]*spriteSessionBuild{}}
	go s.reapLoop(sessionBuildsExpiry)
	return s
}

func (s *spriteSessionBuildStore) add(id string, b *spriteSessionBuild) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.builds[id] = b
}

func (s *spriteSessionBuildStore) get(id string) *spriteSessionBuild {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builds[id]
}

func (s *spriteSessionBuildStore) remove(id string) {
	s.mu.Lock()
	b := s.builds[id]
	delete(s.builds, id)
	s.mu.Unlock()
	if b != nil {
		b.result.Cleanup()
	}
}

func (s *spriteSessionBuildStore) reapLoop(maxAge time.Duration) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-maxAge)
		s.mu.Lock()
		var stale []*spriteSessionBuild
		for id, b := range s.builds {
			if b.createdAt.Before(cutoff) {
				stale = append(stale, b)
				delete(s.builds, id)
			}
		}
		s.mu.Unlock()
		for _, b := range stale {
			b.result.Cleanup()
		}
	}
}

