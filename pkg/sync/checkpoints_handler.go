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
}

type worktreeResponse struct {
	ID                     string    `json:"id"`
	UserID                 string    `json:"user_id"`
	DisplayName            string    `json:"display_name"`
	OwnerKind              OwnerKind `json:"owner_kind"`
	OwnerID                string    `json:"owner_id"`
	LatestSyncedCheckpoint string    `json:"latest_synced_checkpoint,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
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
	writeJSON(w, http.StatusOK, worktreeToResponse(wt))
}

// transferOwnershipRequest is the body of POST /v1/worktrees/{id}/owner.
type transferOwnershipRequest struct {
	ToKind  OwnerKind `json:"to_kind"`
	ToID    string    `json:"to_id"`
	// ExpectedOwnerID guards the optimistic-concurrency check. The
	// caller MUST provide its current view of the row's owner_id; the
	// UPDATE only succeeds if reality still matches. Mismatch returns
	// 409 so the caller can re-read and retry. Empty when caller is
	// local (per-user ownership, no per-device ID); HostID for sprites.
	ExpectedOwnerID string `json:"expected_owner_id"`
}

// handleTransferOwnership performs the atomic ownership transfer for
// MigrateWorktree. Authorization: the caller's Kind must match the
// worktree's current OwnerKind (local↔laptop, remote↔sprite). For
// remote owners the caller's HostID must additionally equal
// worktree.OwnerID. The DB-level UPDATE WHERE owner_id = expected
// catches lost-update races even if the auth check passes.
func (s *Server) handleTransferOwnership(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerOrUnauthorized(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}

	var req transferOwnershipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ToKind != OwnerKindLocal && req.ToKind != OwnerKindRemote {
		http.Error(w, `to_kind must be "local" or "remote"`, http.StatusBadRequest)
		return
	}
	if req.ToID == "" {
		http.Error(w, "to_id is required", http.StatusBadRequest)
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
	// Two flavors of legitimate caller for a transfer:
	//   1. the current owner releasing → routine handoff
	//      (laptop migrating to_sprite).
	//   2. the new owner claiming → reclaim path
	//      (laptop migrating to_laptop after a previous to_sprite,
	//      or another of the user's devices taking over).
	// Both cases represent deliberate user intent within their own
	// tenant. The optimistic-concurrency guard (expected_owner_id)
	// blocks lost-update races regardless of which path applies.
	callerIsCurrentOwner := callerOwnsWorktree(caller, wt)
	callerIsNewOwner := callerMatches(caller, req.ToKind, req.ToID)
	if !callerIsCurrentOwner && !callerIsNewOwner {
		http.Error(w, "caller must be either the current owner or the new owner", http.StatusForbidden)
		return
	}

	expected := req.ExpectedOwnerID
	if expected == "" {
		expected = wt.OwnerID
	}

	if err := s.cfg.Store.UpdateWorktreeOwner(r.Context(), id, wt.OwnerKind, expected, req.ToKind, req.ToID); err != nil {
		if errors.Is(err, ErrOwnerMismatch) {
			http.Error(w, "owner mismatch (concurrent migration?)", http.StatusConflict)
			return
		}
		s.log.Printf("sync: update worktree owner: %v", err)
		http.Error(w, "transfer", http.StatusInternalServerError)
		return
	}

	updated, err := s.cfg.Store.GetWorktreeByID(r.Context(), id)
	if err != nil {
		s.log.Printf("sync: re-read worktree after transfer: %v", err)
		http.Error(w, "transfer", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, worktreeToResponse(updated))
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

	now := time.Now().UTC()
	wt := Worktree{
		ID:          newULID(),
		UserID:      caller.UserID,
		DisplayName: req.DisplayName,
		OwnerKind:   OwnerKindLocal,
		// OwnerID stays empty for local kind — ownership is per-user.
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.cfg.Store.InsertWorktree(r.Context(), wt); err != nil {
		s.log.Printf("sync: insert worktree: %v", err)
		http.Error(w, "insert worktree", http.StatusInternalServerError)
		return
	}

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
	IncrementalCommit string `json:"incremental_commit"`
}

type createCheckpointResponse struct {
	CheckpointID     string    `json:"checkpoint_id"`
	HeadCommitPutURL string    `json:"head_commit_put_url"`
	IncrementalURL   string    `json:"incremental_put_url"`
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
	if !callerOwnsWorktree(caller, wt) {
		http.Error(w, ownerMismatchMessage(caller, wt), http.StatusForbidden)
		return
	}

	result, err := s.CreateCheckpoint(r.Context(), caller.UserID, CreateCheckpointRequest{
		WorktreeID:        req.WorktreeID,
		HeadCommit:        req.HeadCommit,
		HeadRef:           req.HeadRef,
		IndexTree:         req.IndexTree,
		WorktreeTree:      req.WorktreeTree,
		IncrementalCommit: req.IncrementalCommit,
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
		HeadCommitPutURL: result.HeadCommitPutURL,
		IncrementalURL:   result.IncrementalURL,
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

	// Caller-identity authorization (must own the worktree). Tenancy
	// is rechecked inside Server.CommitCheckpoint.
	_, wt, err := s.lookupCheckpointForUser(r.Context(), checkpointID, caller.UserID)
	if err != nil {
		http.Error(w, err.Error(), httpStatusForLookupErr(err))
		return
	}
	if !callerOwnsWorktree(caller, wt) {
		http.Error(w, ownerMismatchMessage(caller, wt), http.StatusForbidden)
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
	IncrementalURL   string `json:"incremental_get_url"`
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
		IncrementalURL:   urls.IncrementalURL,
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

// callerOwnsWorktree returns true when the caller's kind matches the
// worktree's owner kind. Local ownership is per-user (any laptop of
// this user counts as "the owner"); remote ownership additionally
// disambiguates by HostID.
func callerOwnsWorktree(c Caller, wt Worktree) bool {
	switch c.Kind {
	case CallerKindLocal:
		return wt.OwnerKind == OwnerKindLocal
	case CallerKindRemote:
		return wt.OwnerKind == OwnerKindRemote && wt.OwnerID == c.HostID
	default:
		return false
	}
}

// ownerMismatchMessage renders a 403 body that names the actual owner
// and tells the caller what to do — most useful when a laptop tries to
// push to a worktree the remote currently owns.
func ownerMismatchMessage(c Caller, wt Worktree) string {
	switch {
	case c.Kind == CallerKindLocal && wt.OwnerKind == OwnerKindRemote:
		return "not the current owner: worktree is owned by remote (run `clank pull --migrate` to reclaim ownership before pushing again)"
	case c.Kind == CallerKindRemote && wt.OwnerKind == OwnerKindLocal:
		return "not the current owner: worktree is owned by laptop (sprite can only checkpoint while it owns the worktree)"
	}
	return "not the current owner"
}

// callerMatches returns true when the caller's kind equals the
// requested OwnerKind. For OwnerKindLocal, no ID is checked
// (ownership is per-user). For OwnerKindRemote, HostID must match.
// Used by transferOwnership to recognize a legitimate "I'm claiming
// this worktree" reclaim — see handleTransferOwnership.
func callerMatches(c Caller, kind OwnerKind, id string) bool {
	switch kind {
	case OwnerKindLocal:
		return c.Kind == CallerKindLocal
	case OwnerKindRemote:
		return c.Kind == CallerKindRemote && c.HostID == id
	default:
		return false
	}
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

func (s *Server) presignCheckpointPuts(ctx context.Context, userID, worktreeID, checkpointID string) (map[storage.Blob]string, error) {
	out := make(map[storage.Blob]string, 3)
	for _, blob := range []storage.Blob{storage.BlobHeadCommit, storage.BlobIncremental, storage.BlobManifest} {
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
		OwnerKind:              w.OwnerKind,
		OwnerID:                w.OwnerID,
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
// worktree row. Includes the recovery hint for the realistic cause:
// a stale .clank/worktree-id on the laptop pointing at a server row
// that no longer exists (e.g. after a clank-sync DB reset in dev).
func worktreeNotFoundMsg(id string) string {
	return fmt.Sprintf("worktree %s not registered with this clank-sync — if the cached id at <repo>/.clank/worktree-id is stale (e.g. server DB reset), delete it and retry to re-register", id)
}
