package sync

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// sessionPresignRequest is the body of POST /v1/checkpoints/{id}/sessions.
// Sessions are the content-addressed blob refs (externalID + contentHash)
// from the SessionManifest the caller is about to upload.
type sessionPresignRequest struct {
	Sessions []checkpoint.SessionBlobRef `json:"sessions"`
	// SessionsContentDigest is the manifest's ContentDigest over the complete
	// session set; persisted on the checkpoint row. Omitted by old clients ⇒
	// the digest stays unset and autosync fetches the manifest as before.
	SessionsContentDigest string `json:"sessions_content_digest"`
}

type sessionPresignResponse struct {
	CheckpointID          string            `json:"checkpoint_id"`
	SessionPutURLs        map[string]string `json:"session_put_urls"`
	SessionManifestPutURL string            `json:"session_manifest_put_url"`
	TTLSeconds            int               `json:"ttl_seconds"`
}

// handleSessionPresign mints presigned PUT URLs for the per-session
// export blobs + the session-manifest.json sidecar of a checkpoint.
//
// Sits behind POST /v1/checkpoints/{id}/sessions. Caller-identity
// authorization (must own the worktree) mirrors handleCreateCheckpoint
// — the checkpoint must belong to the caller's user AND the caller
// must own the worktree (laptop kind for local pushes, sprite kind
// for sprite-side uploads during materialize).
func (s *Server) handleSessionPresign(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}
	checkpointID := r.PathValue("id")
	if checkpointID == "" {
		http.Error(w, "checkpoint id missing", http.StatusBadRequest)
		return
	}

	var req sessionPresignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	_, _, err := s.lookupCheckpointForUser(r.Context(), checkpointID, caller.UserID)
	if err != nil {
		http.Error(w, "lookup checkpoint: "+err.Error(), httpStatusForLookupErr(err))
		return
	}

	result, err := s.PresignSessionPuts(r.Context(), caller.UserID, SessionPresignRequest{
		CheckpointID:          checkpointID,
		Sessions:              req.Sessions,
		SessionsContentDigest: req.SessionsContentDigest,
	})
	if err != nil {
		s.log.Printf("sync: presign session puts: %v", err)
		switch {
		case errors.Is(err, ErrInvalidRequest):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrForbidden):
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusOK, sessionPresignResponse{
		CheckpointID:          result.CheckpointID,
		SessionPutURLs:        result.SessionPutURLs,
		SessionManifestPutURL: result.SessionManifestPutURL,
		TTLSeconds:            int(s.cfg.PresignTTL.Seconds()),
	})
}
