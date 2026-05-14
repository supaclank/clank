// Package sync is the persistent checkpoint substrate behind the
// gateway-orchestrated MigrateWorktree flow. It mints presigned URLs
// for an S3-compatible object store, tracks worktree + checkpoint
// metadata in a SyncStore, and exposes ownership transitions over
// HTTP. Bundle bytes never traverse the sync server in either
// direction — laptop and gateway upload/download via presigned URLs.
//
// Authentication is the caller's responsibility — mount the handler
// behind pkg/auth.Middleware (or any other middleware that puts an
// auth.Principal in the request context). Sync handlers read
// caller.UserID from that Principal.
package sync

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/acksell/clank/pkg/provisioner/hoststore"
	"github.com/acksell/clank/pkg/sync/storage"
)

// Config configures the sync Server. Store and Storage are required.
type Config struct {
	// Store backs worktree + checkpoint metadata.
	Store SyncStore

	// Storage is the object-storage backend (S3-compatible) where
	// checkpoint bundles live.
	Storage storage.Storage

	// PresignTTL is how long presigned PUT/GET URLs stay valid.
	// Default 5 minutes — long enough for slow uploads, short enough
	// to bound a leaked URL.
	PresignTTL time.Duration

	// CallerVerifier extracts a Caller from inbound requests. Defaults
	// to a stateless HeaderCallerVerifier that reads UserID from the
	// auth.Principal in r.Context() and X-Clank-Host-Id from headers
	// (for sprite callers). Tests can swap in their own verifier.
	CallerVerifier CallerVerifier

	// HostStore enables the sprite-kind cross-check: every sprite-kind
	// caller's claimed host_id is looked up here and the row's user_id
	// is asserted to equal claims.sub. Optional; required when
	// sprite-push lands.
	HostStore hoststore.HostStore
}

// Server is the sync HTTP service.
type Server struct {
	cfg Config
	log *log.Logger
}

// NewServer constructs a sync Server.
func NewServer(cfg Config, lg *log.Logger) (*Server, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("sync: Store is required")
	}
	if cfg.Storage == nil {
		return nil, fmt.Errorf("sync: Storage is required")
	}
	if cfg.PresignTTL == 0 {
		cfg.PresignTTL = 5 * time.Minute
	}
	if cfg.CallerVerifier == nil {
		cfg.CallerVerifier = &HeaderCallerVerifier{}
	}
	if lg == nil {
		lg = log.Default()
	}
	return &Server{cfg: cfg, log: lg}, nil
}

// Handler returns the public HTTP handler. Routes:
//
//	GET  /v1/health                       — liveness (no auth)
//	GET  /v1/worktrees                    — list the caller's worktrees with owner info
//	POST /v1/worktrees                    — register a worktree, returns ID
//	GET  /v1/worktrees/{id}               — read worktree state (gateway uses on migration)
//	POST /v1/worktrees/{id}/owner         — atomic ownership transfer
//	POST /v1/checkpoints                  — create checkpoint metadata, returns presigned PUT URLs
//	POST /v1/checkpoints/{id}/commit      — confirm upload, advance latest_synced_checkpoint
//	GET  /v1/checkpoints/{id}/download    — return presigned GET URLs (gateway uses on migration)
//	POST /v1/checkpoints/{id}/sessions    — mint presigned PUT URLs for per-session export blobs + session-manifest.json
func (s *Server) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/health", s.handleHealth)
	mx.HandleFunc("GET /v1/worktrees", s.handleListWorktrees)
	mx.HandleFunc("POST /v1/worktrees", s.handleRegisterWorktree)
	mx.HandleFunc("GET /v1/worktrees/{id}", s.handleGetWorktree)
	mx.HandleFunc("POST /v1/worktrees/{id}/owner", s.handleTransferOwnership)
	mx.HandleFunc("POST /v1/checkpoints", s.handleCreateCheckpoint)
	mx.HandleFunc("POST /v1/checkpoints/{id}/commit", s.handleCommitCheckpoint)
	mx.HandleFunc("GET /v1/checkpoints/{id}/download", s.handleDownloadCheckpoint)
	mx.HandleFunc("POST /v1/checkpoints/{id}/sessions", s.handleSessionPresign)
	return mx
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
