package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Config configures an S3-compatible Storage backend. Works with
// AWS S3, Cloudflare R2, Tigris, MinIO, and any other S3-compatible
// API by setting Endpoint and UsePathStyle appropriately.
type S3Config struct {
	// Bucket name. Must already exist; we don't auto-create.
	Bucket string

	// Region (required by AWS even for S3-alikes; e.g. R2 wants "auto").
	Region string

	// Endpoint overrides the default AWS S3 endpoint. Set for R2
	// (https://<account>.r2.cloudflarestorage.com), Tigris, MinIO, etc.
	// Leave empty for AWS S3.
	Endpoint string

	// AccessKey + SecretKey for the bucket. Required.
	AccessKey string
	SecretKey string

	// UsePathStyle forces path-style addressing (bucket as URL path
	// segment, not subdomain). Required for MinIO and most R2 setups.
	UsePathStyle bool
}

// S3 is the S3-compatible Storage implementation.
type S3 struct {
	cfg       S3Config
	client    *s3.Client
	presigner *s3.PresignClient
}

// NewS3 constructs an S3 backend. Returns an error if Bucket / Region
// / credentials are missing — fail fast at startup, never silently
// fall back to anonymous access.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage: S3Config.Bucket is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("storage: S3Config.Region is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("storage: S3Config.AccessKey and SecretKey are required")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}
	if cfg.UsePathStyle {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)
	return &S3{
		cfg:       cfg,
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func (s *S3) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign put %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NotFound" {
		return false, nil
	}
	return false, fmt.Errorf("head %s: %w", key, err)
}

// compile-time check
var _ Storage = (*S3)(nil)
