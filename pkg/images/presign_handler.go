package images

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/supaclank/clank/pkg/auth"
	"github.com/supaclank/clank/pkg/blobstore"
	"github.com/oklog/ulid/v2"
)

// presignImageRequest is the body of POST /v1/images.
type presignImageRequest struct {
	Mime     string `json:"mime"`
	Filename string `json:"filename,omitempty"`
}

// presignImageResponse hands the client a content-free upload slot: a
// PUT URL to upload to and a GET URL to embed in the chat message so the
// sprite can download. image_id is the stable, server-minted id.
type presignImageResponse struct {
	ImageID   string `json:"image_id"`
	PutURL    string `json:"put_url"`
	GetURL    string `json:"get_url"`
	ExpiresAt string `json:"expires_at"`
}

// handlePresignImage mints an upload slot for one image. userID comes
// from the authenticated Principal (never request input) so the storage
// key is tenant-scoped. The mime must be in AllowedMimes — fast 400
// otherwise, no fallback.
func (s *Server) handlePresignImage(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB — prevent uncontrolled resource consumption
	var req presignImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !AllowedMimes[req.Mime] {
		http.Error(w, "unsupported image mime: "+req.Mime, http.StatusBadRequest)
		return
	}

	// TODO(ai-review): switch to presigned POST policies (content-length-range + signed Content-Type) if upload-time size/type enforcement is required https://github.com/supaclank/clank/pull/64#discussion_r3438446257
	id, err := ulid.New(ulid.Now(), s.entropySource)
	if err != nil {
		http.Error(w, "generate image id", http.StatusInternalServerError)
		return
	}
	imageID := id.String()
	key, err := KeyForImage(p.UserID, imageID)
	if err != nil {
		http.Error(w, "build image key", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	putURL, err := s.store.PresignPut(ctx, key, s.presignTTL)
	if err != nil {
		writeStorageError(w, "presign put", err)
		return
	}
	getURL, err := s.store.PresignGet(ctx, key, s.presignTTL)
	if err != nil {
		writeStorageError(w, "presign get", err)
		return
	}

	writeJSON(w, http.StatusOK, presignImageResponse{
		ImageID:   imageID,
		PutURL:    putURL,
		GetURL:    getURL,
		ExpiresAt: time.Now().Add(s.presignTTL).UTC().Format(time.RFC3339),
	})
}

// writeStorageError maps a Storage error to a status the client can act
// on: 503 for a temporarily unreachable backing (e.g. the LAN blobstore
// before any phone-reachable address is bound), 500 otherwise.
func writeStorageError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, blobstore.ErrUnavailable) {
		http.Error(w, op+": image uploads aren't reachable from this network yet", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, op, http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
