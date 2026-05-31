package sync

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/acksell/clank/pkg/sync/storage"
	"github.com/oklog/ulid/v2"
)

// registerWorktreeRequest is the body of POST /v1/worktrees.
type registerWorktreeRequest struct {
	DisplayName string `json:"display_name"`
	// OriginRepo identifies the repo this worktree was created from
	// (e.g. "acme/api"). See sync.Worktree.OriginRepo for the field's
	// purpose; optional here for callers that don't have it (older
	// laptops, scripted registrations). Empty value lands the row
	// under "Unknown repo" on grouping clients.
	OriginRepo string `json:"origin_repo,omitempty"`
}

type worktreeResponse struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	// OriginRepo mirrors sync.Worktree.OriginRepo — see that struct's
	// doc. Clients use it as the group key in pickers/sidebars.
	OriginRepo             string `json:"origin_repo,omitempty"`
	LatestSyncedCheckpoint string `json:"latest_synced_checkpoint,omitempty"`
	// LatestCheckpointMetadata carries the 4 content SHAs of the
	// latest synced checkpoint. Populated on single-worktree responses
	// (handleGetWorktree) where the laptop needs to compute drift
	// cheaply. Omitted from list responses
	// (would require a JOIN per row) and when no checkpoint has been
	// pushed yet.
	LatestCheckpointMetadata *checkpointSnapshot `json:"latest_checkpoint_metadata,omitempty"`
	CreatedAt                time.Time           `json:"created_at"`
	UpdatedAt                time.Time           `json:"updated_at"`
}

// checkpointSnapshot is the subset of Checkpoint fields the laptop
// needs for divergence detection: the same content SHAs the local
// checkpoint.Manifest carries. Letting the laptop compare locally
// avoids fetching the full manifest blob from S3 just to check
// "is my local state already synced?".
type checkpointSnapshot struct {
	CheckpointID      string `json:"checkpoint_id"`
	HeadCommit        string `json:"head_commit"`
	HeadRef           string `json:"head_ref,omitempty"`
	IndexTree         string `json:"index_tree"`
	WorktreeTree      string `json:"worktree_tree"`
	UncommittedCommit string `json:"uncommitted_commit"`
}

// handleListWorktrees returns all worktrees belonging to the caller.
// Used by the TUI sidebar to render ownership state per worktree.
func (s *Server) handleListWorktrees(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}
	wts, err := s.ListWorktrees(r.Context(), caller.UserID)
	if err != nil {
		s.log.Printf("sync: list worktrees: %v", err)
		http.Error(w, "list worktrees", http.StatusInternalServerError)
		return
	}
	out := make([]worktreeResponse, 0, len(wts))
	for _, wt := range wts {
		out = append(out, worktreeToResponse(wt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": out})
}

// handleGetWorktree returns the worktree row to its owning user.
// Used by the gateway during MigrateWorktree to read
// latest_synced_checkpoint and validate ownership.
func (s *Server) handleGetWorktree(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}
	wt, err := s.cfg.Store.GetWorktreeByID(r.Context(), id)
	if errors.Is(err, ErrWorktreeNotFound) {
		http.Error(w, worktreeNotFoundMsg(id), http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Printf("sync: get worktree: %v", err)
		http.Error(w, "lookup worktree", http.StatusInternalServerError)
		return
	}
	if wt.UserID != caller.UserID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	resp := worktreeToResponse(wt)
	s.attachCheckpointSnapshot(r.Context(), &resp, wt.LatestSyncedCheckpoint)
	writeJSON(w, http.StatusOK, resp)
}

// attachCheckpointSnapshot enriches a worktree response with the
// 4 content SHAs of the named checkpoint. Best-effort: a lookup
// failure is logged and the response goes out without the field
// (clients treat missing-snapshot as "treat as diverged", which is
// the safe default).
func (s *Server) attachCheckpointSnapshot(ctx context.Context, resp *worktreeResponse, checkpointID string) {
	if checkpointID == "" {
		return
	}
	ck, err := s.cfg.Store.GetCheckpointByID(ctx, checkpointID)
	if err != nil {
		s.log.Printf("sync: snapshot checkpoint %s: %v", checkpointID, err)
		return
	}
	resp.LatestCheckpointMetadata = &checkpointSnapshot{
		CheckpointID:      ck.ID,
		HeadCommit:        ck.HeadCommit,
		HeadRef:           ck.HeadRef,
		IndexTree:         ck.IndexTree,
		WorktreeTree:      ck.WorktreeTree,
		UncommittedCommit: ck.UncommittedCommit,
	}
}

func (s *Server) handleRegisterWorktree(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}
	if caller.Kind != CallerKindLocal {
		http.Error(w, "only laptop callers may register worktrees", http.StatusForbidden)
		return
	}

	var req registerWorktreeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.DisplayName == "" {
		http.Error(w, "display_name is required", http.StatusBadRequest)
		return
	}

	// ID is an opaque ULID; the human-readable display_name is kept
	// separate so `clank status`, S3 paths, and sprite directories can
	// still surface a memorable label without the ID needing to be one.
	now := time.Now().UTC()
	wt := Worktree{
		ID:          newULID(),
		UserID:      caller.UserID,
		DisplayName: req.DisplayName,
		OriginRepo:  req.OriginRepo,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.cfg.Store.InsertWorktree(r.Context(), wt); err != nil {
		s.log.Printf("sync: insert worktree: %v", err)
		http.Error(w, "insert worktree", http.StatusInternalServerError)
		return
	}
	// Return the row we just inserted. A defensive re-read here would
	// turn any transient Get failure into a 500 *after* the row was
	// successfully created.
	writeJSON(w, http.StatusCreated, worktreeToResponse(wt))
}

// createCheckpointRequest is the body of POST /v1/checkpoints. Field
// shapes match checkpoint.Manifest minus the server-assigned ID and
// CreatedAt/By. Caller identity (userID + host_id for sprites) comes
// from CallerVerifier, not the request body.
type createCheckpointRequest struct {
	WorktreeID        string `json:"worktree_id"`
	HeadCommit        string `json:"head_commit"`
	HeadRef           string `json:"head_ref"`
	IndexTree         string `json:"index_tree"`
	WorktreeTree      string `json:"worktree_tree"`
	UncommittedCommit string `json:"uncommitted_commit"`
}

type createCheckpointResponse struct {
	CheckpointID string `json:"checkpoint_id"`
	// HeadBundleAction is already_stored | upload_full | upload_incremental.
	// HeadBundlePutURL is set unless already_stored; HeadBundleBase is set
	// only for upload_incremental.
	HeadBundleAction string    `json:"head_bundle_action"`
	HeadBundlePutURL string    `json:"head_bundle_put_url,omitempty"`
	HeadBundleBase   string    `json:"head_bundle_base,omitempty"`
	UncommittedURL   string    `json:"uncommitted_put_url"`
	ManifestPutURL   string    `json:"manifest_put_url"`
	TTLSeconds       int       `json:"ttl_seconds"`
	CreatedAt        time.Time `json:"created_at"`
}

func (s *Server) handleCreateCheckpoint(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}

	var req createCheckpointRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Caller-identity check (laptop/sprite ownership) lives at this
	// layer; tenancy is the service method's concern. Look up the
	// worktree once here, then pass through.
	wt, err := s.cfg.Store.GetWorktreeByID(r.Context(), req.WorktreeID)
	if errors.Is(err, ErrWorktreeNotFound) {
		http.Error(w, worktreeNotFoundMsg(req.WorktreeID), http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Printf("sync: get worktree: %v", err)
		http.Error(w, "lookup worktree", http.StatusInternalServerError)
		return
	}
	if wt.UserID != caller.UserID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	result, err := s.CreateCheckpoint(r.Context(), caller.UserID, CreateCheckpointRequest{
		WorktreeID:        req.WorktreeID,
		HeadCommit:        req.HeadCommit,
		HeadRef:           req.HeadRef,
		IndexTree:         req.IndexTree,
		WorktreeTree:      req.WorktreeTree,
		UncommittedCommit: req.UncommittedCommit,
		CreatedBy:         createdByFor(caller),
	})
	if err != nil {
		s.log.Printf("sync: create checkpoint: %v", err)
		switch {
		case errors.Is(err, ErrInvalidRequest):
			// Validation message is public-facing — tells the client
			// which required fields are missing.
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrForbidden):
			// Defense-in-depth: handler already gated tenancy above, but
			// the service re-checks. Map to 403 if it ever fires.
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusCreated, createCheckpointResponse{
		CheckpointID:     result.CheckpointID,
		HeadBundleAction: string(result.HeadBundleAction),
		HeadBundlePutURL: result.HeadBundlePutURL,
		HeadBundleBase:   result.HeadBundleBase,
		UncommittedURL:   result.UncommittedURL,
		ManifestPutURL:   result.ManifestPutURL,
		TTLSeconds:       int(result.PresignTTL.Seconds()),
		CreatedAt:        result.CreatedAt,
	})
}

type commitCheckpointResponse struct {
	CheckpointID string    `json:"checkpoint_id"`
	UploadedAt   time.Time `json:"uploaded_at"`
}

func (s *Server) handleCommitCheckpoint(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}
	checkpointID := r.PathValue("id")
	if checkpointID == "" {
		http.Error(w, "checkpoint id missing", http.StatusBadRequest)
		return
	}

	// Tenancy is rechecked inside Server.CommitCheckpoint.
	_, _, err := s.lookupCheckpointForUser(r.Context(), checkpointID, caller.UserID)
	if err != nil {
		http.Error(w, err.Error(), httpStatusForLookupErr(err))
		return
	}

	result, err := s.CommitCheckpoint(r.Context(), caller.UserID, checkpointID)
	if err != nil {
		s.log.Printf("sync: commit checkpoint: %v", err)
		switch {
		case errors.Is(err, ErrBlobNotUploaded):
			// Blob list is public-facing — tells the client which blob
			// they still need to upload before retrying.
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrForbidden):
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusOK, commitCheckpointResponse{
		CheckpointID: result.CheckpointID,
		UploadedAt:   result.UploadedAt,
	})
}

type downloadCheckpointResponse struct {
	CheckpointID     string `json:"checkpoint_id"`
	HeadCommitGetURL string `json:"head_commit_get_url"`
	UncommittedURL   string `json:"uncommitted_get_url"`
	ManifestGetURL   string `json:"manifest_get_url"`
	TTLSeconds       int    `json:"ttl_seconds"`
}

// handleDownloadCheckpoint returns presigned GET URLs for the gateway
// to fetch bundle bytes during MigrateWorktree (P3+). Authorized to
// the checkpoint's owning user.
func (s *Server) handleDownloadCheckpoint(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}
	checkpointID := r.PathValue("id")
	if checkpointID == "" {
		http.Error(w, "checkpoint id missing", http.StatusBadRequest)
		return
	}

	urls, err := s.DownloadCheckpointURLs(r.Context(), caller.UserID, checkpointID)
	if err != nil {
		s.log.Printf("sync: download urls: %v", err)
		http.Error(w, err.Error(), httpStatusForLookupErr(err))
		return
	}
	writeJSON(w, http.StatusOK, downloadCheckpointResponse{
		CheckpointID:     urls.CheckpointID,
		HeadCommitGetURL: urls.HeadCommitGetURL,
		UncommittedURL:   urls.UncommittedURL,
		ManifestGetURL:   urls.ManifestGetURL,
		TTLSeconds:       int(s.cfg.PresignTTL.Seconds()),
	})
}

func (s *Server) callerOrUnauthorized(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	c, err := s.cfg.CallerVerifier.VerifyCaller(r)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoPrincipal):
			// Server misconfiguration — outer auth middleware didn't run.
			s.log.Printf("sync: %v (auth middleware not wired?)", err)
			http.Error(w, "internal misconfiguration: no auth principal", http.StatusInternalServerError)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="clank-sync"`)
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		}
		return Caller{}, false
	}
	if c.Kind == CallerKindRemote {
		// Without a HostStore the cross-tenant guard cannot run. Refuse
		// remote-kind callers entirely rather than silently bypassing
		// the check and trusting the X-Clank-Host-Id header alone.
		if s.cfg.HostStore == nil {
			s.log.Printf("sync: rejecting remote caller (host_id=%s, user=%s): HostStore not configured", c.HostID, c.UserID)
			http.Error(w, "remote callers not enabled on this server", http.StatusForbidden)
			return Caller{}, false
		}
		host, err := s.cfg.HostStore.GetHostByID(r.Context(), c.HostID)
		if err != nil {
			http.Error(w, "unknown sprite host", http.StatusUnauthorized)
			return Caller{}, false
		}
		if host.UserID != c.UserID {
			s.log.Printf("sync: sprite cross-check failed: host_id=%s claims user=%s host user=%s", c.HostID, c.UserID, host.UserID)
			http.Error(w, "sprite/user mismatch", http.StatusForbidden)
			return Caller{}, false
		}
	}
	return c, true
}

// createdByFor returns the canonical CreatedBy stamp for a caller.
func createdByFor(c Caller) string {
	switch c.Kind {
	case CallerKindLocal:
		return "laptop:" + c.UserID
	case CallerKindRemote:
		return "sprite:" + c.HostID
	default:
		return string(c.Kind) + ":" + c.UserID
	}
}

// lookupCheckpointForUser fetches a checkpoint and its worktree,
// asserting the worktree belongs to userID. Distinct error returns
// drive the right HTTP status via httpStatusForLookupErr.
func (s *Server) lookupCheckpointForUser(ctx context.Context, checkpointID, userID string) (Checkpoint, Worktree, error) {
	ck, err := s.cfg.Store.GetCheckpointByID(ctx, checkpointID)
	if errors.Is(err, ErrCheckpointNotFound) {
		return Checkpoint{}, Worktree{}, errCheckpointNotFound
	}
	if err != nil {
		s.log.Printf("sync: get checkpoint: %v", err)
		return Checkpoint{}, Worktree{}, errLookupInternal
	}
	wt, err := s.cfg.Store.GetWorktreeByID(ctx, ck.WorktreeID)
	if errors.Is(err, ErrWorktreeNotFound) {
		return Checkpoint{}, Worktree{}, errWorktreeNotFound
	}
	if err != nil {
		s.log.Printf("sync: get worktree: %v", err)
		return Checkpoint{}, Worktree{}, errLookupInternal
	}
	if wt.UserID != userID {
		return Checkpoint{}, Worktree{}, errForbidden
	}
	return ck, wt, nil
}

var (
	errCheckpointNotFound = errors.New("checkpoint not found")
	errWorktreeNotFound   = errors.New("worktree not found")
	errForbidden          = errors.New("forbidden")
	errLookupInternal     = errors.New("internal")
)

func httpStatusForLookupErr(err error) int {
	switch {
	case errors.Is(err, errCheckpointNotFound), errors.Is(err, errWorktreeNotFound):
		return http.StatusNotFound
	case errors.Is(err, errForbidden):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// presignCheckpointPuts mints PUT URLs for the per-checkpoint blobs
// (uncommitted bundle + manifest). The head bundle is content-addressed
// and handled separately by CreateCheckpoint via storage.KeyForHead.
func (s *Server) presignCheckpointPuts(ctx context.Context, userID, worktreeID, checkpointID string) (map[storage.Blob]string, error) {
	out := make(map[storage.Blob]string, 2)
	for _, blob := range []storage.Blob{storage.BlobUncommitted, storage.BlobManifest} {
		key, err := storage.KeyFor(userID, worktreeID, checkpointID, blob)
		if err != nil {
			return nil, fmt.Errorf("key for %s: %w", blob, err)
		}
		u, err := s.cfg.Storage.PresignPut(ctx, key, s.cfg.PresignTTL)
		if err != nil {
			return nil, fmt.Errorf("presign %s: %w", blob, err)
		}
		out[blob] = u
	}
	return out, nil
}

func worktreeToResponse(w Worktree) worktreeResponse {
	return worktreeResponse{
		ID:                     w.ID,
		UserID:                 w.UserID,
		DisplayName:            w.DisplayName,
		OriginRepo:             w.OriginRepo,
		LatestSyncedCheckpoint: w.LatestSyncedCheckpoint,
		CreatedAt:              w.CreatedAt,
		UpdatedAt:              w.UpdatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newULID() string {
	return ulid.MustNew(ulid.Now(), cryptorand.Reader).String()
}

// worktreeNotFoundMsg formats the user-facing 404 body for a missing
// worktree row.
func worktreeNotFoundMsg(id string) string {
	return fmt.Sprintf("worktree %s not registered with this clank-sync", id)
}
