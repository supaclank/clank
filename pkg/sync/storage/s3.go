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
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// deleteBatchSize is the S3 DeleteObjects per-request cap.
const deleteBatchSize = 1000

// S3Config configures an S3-compatible Storage backend. Works with
// AWS S3, Cloudflare R2, Tigris, MinIO, and any other S3-compatible
// API by setting Endpoint and UsePathStyle appropriately.
type S3Config struct {
	// Bucket name. Must already exist; we don't auto-create.
	Bucket string

	// Region (required by AWS even for S3-alikes; e.g. R2 wants "auto").
	Region string

	// Endpoint is the URL the gateway uses for its own direct SDK calls
	// (HeadObject during checkpoint commit, etc.). Should be reachable
	// from inside the gateway — for docker-compose dev that's the
	// internal docker hostname like http://clank-minio:9000. Leave
	// empty for AWS S3.
	Endpoint string

	// PublicEndpoint is the URL baked into presigned URLs handed out to
	// the laptop and any remote sprite. Must resolve from BOTH ends to
	// the same backing storage (because SigV4 binds the host into the
	// signature). When empty, falls back to Endpoint.
	//
	// Why two endpoints: the docker dev stack wraps minio behind a
	// Cloudflare quick tunnel so a fly.io sprite can pull from it; the
	// tunnel rewrites the Host header on inbound requests, which breaks
	// SigV4 if the gateway itself goes through it. The gateway short-
	// circuits to the docker-internal hostname for its own calls while
	// minting presigned URLs with the tunnel hostname.
	PublicEndpoint string

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

	// aws-sdk-go-v2/service/s3 v1.x defaults to "WhenSupported" for both
	// request-checksum-calc and response-checksum-validation, which means
	// the SDK adds x-amz-checksum-* / x-amz-checksum-mode headers on PUT
	// and HEAD. MinIO recent releases accept PUT with these but reject
	// HEAD with 403 SignatureDoesNotMatch when the checksum-mode header
	// gets included in the signed canonical request. R2 / Tigris have
	// similar quirks. Forcing WhenRequired keeps the SDK out of the
	// checksum-extension business unless an operation strictly needs it
	// (which our manifest+bundle puts don't).
	commonOpts := func(o *s3.Options) {
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		if cfg.UsePathStyle {
			o.UsePathStyle = true
		}
	}

	internalEndpoint := cfg.Endpoint
	publicEndpoint := cfg.PublicEndpoint
	if publicEndpoint == "" {
		publicEndpoint = internalEndpoint
	}

	directOpts := []func(*s3.Options){commonOpts}
	if internalEndpoint != "" {
		directOpts = append(directOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(internalEndpoint)
		})
	}
	presignOpts := []func(*s3.Options){commonOpts}
	if publicEndpoint != "" {
		presignOpts = append(presignOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(publicEndpoint)
		})
	}

	client := s3.NewFromConfig(awsCfg, directOpts...)
	// Use a separate client for presigning so URLs are signed for the
	// public hostname even when the gateway dials the internal one.
	presignClient := s3.NewFromConfig(awsCfg, presignOpts...)
	return &S3{
		cfg:       cfg,
		client:    client,
		presigner: s3.NewPresignClient(presignClient),
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

// DeletePrefix lists every object under prefix and deletes them in
// batches of deleteBatchSize. Paginates on the continuation token until
// the listing is exhausted. Uses the direct (internal-endpoint) client,
// not the presigner. A NoSuchKey on an individual object is treated as
// already-gone; only a hard error aborts (the caller retries — the
// operation is idempotent on the same prefix).
func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	var token *string
	for {
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		ids := make([]s3types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			ids = append(ids, s3types.ObjectIdentifier{Key: obj.Key})
		}
		for start := 0; start < len(ids); start += deleteBatchSize {
			end := min(start+deleteBatchSize, len(ids))
			out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(s.cfg.Bucket),
				Delete: &s3types.Delete{Objects: ids[start:end], Quiet: aws.Bool(true)},
			})
			if err != nil {
				return fmt.Errorf("delete batch under %s: %w", prefix, err)
			}
			for _, e := range out.Errors {
				if code := aws.ToString(e.Code); code != "NoSuchKey" {
					return fmt.Errorf("delete %s under %s: %s %s", aws.ToString(e.Key), prefix, code, aws.ToString(e.Message))
				}
			}
		}
		if aws.ToBool(page.IsTruncated) {
			token = page.NextContinuationToken
			continue
		}
		return nil
	}
}

// compile-time check
var _ Storage = (*S3)(nil)
