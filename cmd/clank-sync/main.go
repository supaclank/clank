// clank-sync is the persistent checkpoint substrate. It mints
// presigned URLs for an S3-compatible object store, tracks worktree +
// checkpoint metadata in SQLite, and exposes ownership transitions
// over HTTP. Bundle bytes never traverse this process — laptops and
// the gateway upload/download via the presigned URLs directly.
//
// Env (laptop dev / single-tenant deployment):
//
//	CLANK_SYNC_LISTEN_ADDR  — HTTP listen (default ":8081")
//	CLANK_SYNC_DB_PATH      — SQLite path for the SyncStore (default
//	                           ~/.local/state/clank/clank.db)
//
// Object storage (S3-compatible — AWS S3, R2, Tigris, MinIO).
// All four credentials are required.
//
//	CLANK_SYNC_S3_BUCKET     — bucket name
//	CLANK_SYNC_S3_REGION     — region (use "auto" for R2)
//	CLANK_SYNC_S3_ENDPOINT   — optional; set for R2/Tigris/MinIO
//	CLANK_SYNC_S3_ACCESS_KEY — access key
//	CLANK_SYNC_S3_SECRET_KEY — secret key
//	CLANK_SYNC_S3_PATH_STYLE — "1"/"true" for path-style addressing
//	                            (required for MinIO and most R2 setups)
//	CLANK_SYNC_PRESIGN_TTL   — Go duration; default "5m"
//
// For multi-tenant cloud deployments, run this binary with the same
// env your cloud gateway uses; auth defaults to permissive (PR
// follow-up: add a Supabase verifier flag here, or wrap pkg/sync.Server
// with its own auth layer in a separate binary).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/store"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

const defaultListenAddr = ":8081"

func main() {
	addr := envOrDefault("CLANK_SYNC_LISTEN_ADDR", defaultListenAddr)

	dbPath := os.Getenv("CLANK_SYNC_DB_PATH")
	if dbPath == "" {
		var err error
		dbPath, err = defaultDBPath()
		if err != nil {
			log.Fatalf("default db path: %v", err)
		}
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store %s: %v", dbPath, err)
	}
	defer st.Close()

	s3Cfg := mustLoadS3Config()
	ctxInit, cancelInit := context.WithTimeout(context.Background(), 30*time.Second)
	bucket, err := storage.NewS3(ctxInit, s3Cfg)
	cancelInit()
	if err != nil {
		log.Fatalf("storage S3: %v", err)
	}

	syncCfg := clanksync.Config{
		Store:   st,
		Storage: bucket,
	}
	if ttl := os.Getenv("CLANK_SYNC_PRESIGN_TTL"); ttl != "" {
		parsed, err := time.ParseDuration(ttl)
		if err != nil {
			log.Fatalf("CLANK_SYNC_PRESIGN_TTL: %v", err)
		}
		syncCfg.PresignTTL = parsed
	}

	srv, err := clanksync.NewServer(syncCfg, log.Default())
	if err != nil {
		log.Fatalf("sync server: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("clank-sync listening on %s (bucket=%s region=%s)", addr, s3Cfg.Bucket, s3Cfg.Region)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// defaultDBPath returns the SQLite path for the SyncStore. Honors
// XDG_STATE_HOME when set (Linux/XDG-compliant); otherwise resolves to
// ~/.local/state/clank/clank.db. Server state belongs in a state dir,
// not a config dir — keeps the path stable across users who only set
// XDG_CONFIG_HOME, and makes "where do I find/back up the db" obvious.
func defaultDBPath() (string, error) {
	var base string
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		base = v
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "clank.db"), nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// mustLoadS3Config reads CLANK_SYNC_S3_* env vars and exits if any
// required field is missing. clank-sync has no useful behavior without
// object storage; failing fast is preferable to silent half-config.
func mustLoadS3Config() storage.S3Config {
	bucket := mustEnv("CLANK_SYNC_S3_BUCKET")
	region := mustEnv("CLANK_SYNC_S3_REGION")
	access := mustEnv("CLANK_SYNC_S3_ACCESS_KEY")
	secret := mustEnv("CLANK_SYNC_S3_SECRET_KEY")
	pathStyle := false
	if v := os.Getenv("CLANK_SYNC_S3_PATH_STYLE"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			log.Fatalf("CLANK_SYNC_S3_PATH_STYLE: %v", err)
		}
		pathStyle = parsed
	}
	return storage.S3Config{
		Bucket:       bucket,
		Region:       region,
		Endpoint:     os.Getenv("CLANK_SYNC_S3_ENDPOINT"),
		AccessKey:    access,
		SecretKey:    secret,
		UsePathStyle: pathStyle,
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s must be set", key)
	}
	return v
}
