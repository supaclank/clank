// Package sync is the persistent checkpoint substrate behind the
// gateway-orchestrated MigrateWorktree flow. It mints presigned URLs
// for an S3-compatible object store, tracks worktree + checkpoint
// metadata in a SyncStore, and exposes ownership transitions over
// HTTP. Bundle bytes never traverse the sync server in either
// direction — laptop and gateway upload/download via presigned URLs.
//
// Auth is pluggable — pass an Authenticator. The library provides
// PermissiveAuth (any bearer accepted) for laptop-local dev; production
// wraps the cloud's JWT verifier.
package sync

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/acksell/clank/pkg/provisioner/hoststore"
	"github.com/acksell/clank/pkg/sync/storage"
)

// Authenticator verifies the inbound bearer token and returns claims.
// The userID extractor (Config.UserIDFromClaims) maps those claims to
// the userID downstream code expects.
type Authenticator interface {
	Verify(r *http.Request) (claims map[string]any, err error)
}

// PermissiveAuth accepts every request. Useful for laptop-local dev;
// do not deploy publicly without replacing it.
type PermissiveAuth struct{}

func (PermissiveAuth) Verify(*http.Request) (map[string]any, error) {
	return map[string]any{"sub": "local"}, nil
}

// Config configures the sync Server. Store and Storage are required.
type Config struct {
	// Auth verifies the inbound bearer. Defaults to PermissiveAuth.
	Auth Authenticator

	// UserIDFromClaims maps the Authenticator's claim map to the
	// per-tenant userID. Defaults to claims["sub"].
	UserIDFromClaims func(claims map[string]any) (string, error)

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
	// to a HeaderCallerVerifier wrapping Auth + UserIDFromClaims with
	// X-Clank-Device-Id / X-Clank-Host-Id headers. Production
	// deployments will plug in a JWT verifier that puts those values
	// in claims directly.
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
	if cfg.Auth == nil {
		cfg.Auth = PermissiveAuth{}
	}
	if cfg.UserIDFromClaims == nil {
		cfg.UserIDFromClaims = func(claims map[string]any) (string, error) {
			if s, ok := claims["sub"].(string); ok && s != "" {
				return s, nil
			}
			return "", fmt.Errorf("no sub claim")
		}
	}
	if cfg.PresignTTL == 0 {
		cfg.PresignTTL = 5 * time.Minute
	}
	if cfg.CallerVerifier == nil {
		cfg.CallerVerifier = &HeaderCallerVerifier{
			Auth:             cfg.Auth,
			UserIDFromClaims: cfg.UserIDFromClaims,
		}
	}
	if lg == nil {
		lg = log.Default()
	}
	return &Server{cfg: cfg, log: lg}, nil
}

// Handler returns the public HTTP handler. Routes:
//
//	GET  /v1/health                       — liveness (no auth)
//	POST /v1/worktrees                    — register a worktree, returns ID
//	GET  /v1/worktrees/{id}               — read worktree state (gateway uses on migration)
//	POST /v1/worktrees/{id}/owner         — atomic ownership transfer
//	POST /v1/checkpoints                  — create checkpoint metadata, returns presigned PUT URLs
//	POST /v1/checkpoints/{id}/commit      — confirm upload, advance latest_synced_checkpoint
//	GET  /v1/checkpoints/{id}/download    — return presigned GET URLs (gateway uses on migration)
func (s *Server) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/health", s.handleHealth)
	mx.HandleFunc("POST /v1/worktrees", s.handleRegisterWorktree)
	mx.HandleFunc("GET /v1/worktrees/{id}", s.handleGetWorktree)
	mx.HandleFunc("POST /v1/worktrees/{id}/owner", s.handleTransferOwnership)
	mx.HandleFunc("POST /v1/checkpoints", s.handleCreateCheckpoint)
	mx.HandleFunc("POST /v1/checkpoints/{id}/commit", s.handleCommitCheckpoint)
	mx.HandleFunc("GET /v1/checkpoints/{id}/download", s.handleDownloadCheckpoint)
	return mx
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
