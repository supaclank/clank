package daemoncli

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/acksell/clank/internal/store"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// loadSyncFromEnv builds a *clanksync.Server from CLANK_SYNC_S3_* env
// vars. Returns (nil, nil) when CLANK_SYNC_S3_BUCKET is unset — that's
// laptop mode (no sync routes on the gateway). Returns an error if the
// bucket is set but any required companion var is missing or any value
// is malformed, so misconfigurations fail loudly at startup.
//
// Required when sync is enabled:
//
//	CLANK_SYNC_S3_BUCKET
//	CLANK_SYNC_S3_REGION
//	CLANK_SYNC_S3_ACCESS_KEY
//	CLANK_SYNC_S3_SECRET_KEY
//
// Optional:
//
//	CLANK_SYNC_S3_ENDPOINT        — backing-storage URL the gateway dials
//	                                directly (e.g. http://clank-minio:9000)
//	CLANK_SYNC_S3_PUBLIC_ENDPOINT — URL baked into presigned URLs handed
//	                                to laptop + sprite. Falls back to
//	                                CLANK_SYNC_S3_ENDPOINT when unset.
//	                                Set this to the cloudflared tunnel URL
//	                                so a remote sprite can pull.
//	CLANK_SYNC_S3_PATH_STYLE      — "1"/"true" for path-style addressing
//	CLANK_SYNC_DB_PATH            — SQLite path (default ~/.local/state/clank/clank.db)
//	CLANK_SYNC_PRESIGN_TTL        — Go duration, default 5m
func loadSyncFromEnv(ctx context.Context, lg *log.Logger) (*clanksync.Server, error) {
	bucket := os.Getenv("CLANK_SYNC_S3_BUCKET")
	if bucket == "" {
		return nil, nil
	}

	region, err := requireEnv("CLANK_SYNC_S3_REGION")
	if err != nil {
		return nil, err
	}
	access, err := requireEnv("CLANK_SYNC_S3_ACCESS_KEY")
	if err != nil {
		return nil, err
	}
	secret, err := requireEnv("CLANK_SYNC_S3_SECRET_KEY")
	if err != nil {
		return nil, err
	}
	pathStyle := false
	if v := os.Getenv("CLANK_SYNC_S3_PATH_STYLE"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CLANK_SYNC_S3_PATH_STYLE: %w", err)
		}
		pathStyle = parsed
	}

	dbPath := os.Getenv("CLANK_SYNC_DB_PATH")
	if dbPath == "" {
		dbPath, err = defaultSyncDBPath()
		if err != nil {
			return nil, fmt.Errorf("default sync db path: %w", err)
		}
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sync store %s: %w", dbPath, err)
	}

	s3Cfg := storage.S3Config{
		Bucket:         bucket,
		Region:         region,
		Endpoint:       os.Getenv("CLANK_SYNC_S3_ENDPOINT"),
		PublicEndpoint: os.Getenv("CLANK_SYNC_S3_PUBLIC_ENDPOINT"),
		AccessKey:      access,
		SecretKey:      secret,
		UsePathStyle:   pathStyle,
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bkt, err := storage.NewS3(initCtx, s3Cfg)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("init S3 storage: %w", err)
	}

	syncCfg := clanksync.Config{
		Store:   st,
		Storage: bkt,
	}
	if ttl := os.Getenv("CLANK_SYNC_PRESIGN_TTL"); ttl != "" {
		parsed, err := time.ParseDuration(ttl)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("CLANK_SYNC_PRESIGN_TTL: %w", err)
		}
		syncCfg.PresignTTL = parsed
	}

	srv, err := clanksync.NewServer(syncCfg, lg)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("build sync server: %w", err)
	}
	return srv, nil
}

// defaultSyncDBPath resolves the SQLite path for the embedded sync
// store when CLANK_SYNC_DB_PATH is unset. Honors XDG_STATE_HOME when
// set; otherwise ~/.local/state/clank/clank.db. Server state belongs
// in a state dir, not a config dir.
func defaultSyncDBPath() (string, error) {
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

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s must be set when CLANK_SYNC_S3_BUCKET is set", key)
	}
	return v, nil
}
