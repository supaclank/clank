package daemoncli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/images"
)

// loadImagesFromEnv builds a *images.Server from CLANK_IMAGES_S3_* env
// vars. Returns (nil, nil) when CLANK_IMAGES_S3_BUCKET is unset — image
// uploads are simply disabled (POST /v1/images 404s). Returns an error
// if the bucket is set but a required companion var is missing or
// malformed, so misconfigurations fail loudly at startup.
//
// Images use a FULLY INDEPENDENT bucket from sync — different concern,
// different blast radius — so the config block is its own, never derived
// from CLANK_SYNC_S3_*. Required when enabled:
//
//	CLANK_IMAGES_S3_BUCKET
//	CLANK_IMAGES_S3_REGION
//	CLANK_IMAGES_S3_ACCESS_KEY
//	CLANK_IMAGES_S3_SECRET_KEY
//
// Optional:
//
//	CLANK_IMAGES_S3_ENDPOINT        — backing-storage URL the gateway dials
//	                                  directly (e.g. http://clank-minio:9000)
//	CLANK_IMAGES_S3_PUBLIC_ENDPOINT — URL baked into presigned URLs. The
//	                                  sprite must resolve it to download.
//	                                  Falls back to CLANK_IMAGES_S3_ENDPOINT.
//	CLANK_IMAGES_S3_PATH_STYLE      — "1"/"true" for path-style addressing
//	CLANK_IMAGES_PRESIGN_TTL        — Go duration, default 30m
func loadImagesFromEnv(ctx context.Context) (*images.Server, error) {
	bucket := os.Getenv("CLANK_IMAGES_S3_BUCKET")
	if bucket == "" {
		return nil, nil
	}

	region, err := requireEnv("CLANK_IMAGES_S3_REGION", "CLANK_IMAGES_S3_BUCKET")
	if err != nil {
		return nil, err
	}
	access, err := requireEnv("CLANK_IMAGES_S3_ACCESS_KEY", "CLANK_IMAGES_S3_BUCKET")
	if err != nil {
		return nil, err
	}
	secret, err := requireEnv("CLANK_IMAGES_S3_SECRET_KEY", "CLANK_IMAGES_S3_BUCKET")
	if err != nil {
		return nil, err
	}
	pathStyle := false
	if v := os.Getenv("CLANK_IMAGES_S3_PATH_STYLE"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CLANK_IMAGES_S3_PATH_STYLE: %w", err)
		}
		pathStyle = parsed
	}

	s3Cfg := blobstore.S3Config{
		Bucket:         bucket,
		Region:         region,
		Endpoint:       os.Getenv("CLANK_IMAGES_S3_ENDPOINT"),
		PublicEndpoint: os.Getenv("CLANK_IMAGES_S3_PUBLIC_ENDPOINT"),
		AccessKey:      access,
		SecretKey:      secret,
		UsePathStyle:   pathStyle,
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bkt, err := blobstore.NewS3(initCtx, s3Cfg)
	if err != nil {
		return nil, fmt.Errorf("init images S3 storage: %w", err)
	}

	imgCfg := images.Config{Storage: bkt}
	if ttl := os.Getenv("CLANK_IMAGES_PRESIGN_TTL"); ttl != "" {
		parsed, err := time.ParseDuration(ttl)
		if err != nil {
			return nil, fmt.Errorf("CLANK_IMAGES_PRESIGN_TTL: %w", err)
		}
		imgCfg.PresignTTL = parsed
	}

	srv, err := images.NewServer(imgCfg)
	if err != nil {
		return nil, fmt.Errorf("build images server: %w", err)
	}
	return srv, nil
}
