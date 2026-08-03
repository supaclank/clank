// Package images is the gateway-side presign service for user image
// uploads: its own blobstore.Storage (its own bucket) and its own
// /v1/images route, built on pkg/blobstore.
//
// Flow (see the project plan): the client asks for an upload slot and
// gets a presigned PUT URL plus a presigned GET URL; it uploads the
// bytes, then sends the GET URL inside its chat message. The
// credential-free sprite downloads via the GET URL and inlines the
// image into the agent — the gateway never sees the bytes.
package images

import (
	"errors"
	"io"
	"net/http"
	"time"

	cryptorand "crypto/rand"

	"github.com/supaclank/clank/pkg/blobstore"
)

// DefaultPresignTTL is how long image presigned URLs stay valid when
// Config.PresignTTL is zero. Longer than sync's 5m default because the
// GET URL is minted at upload time and must outlive the user composing
// and sending the message.
const DefaultPresignTTL = 30 * time.Minute

// AllowedMimes is the closed set of accepted image content types — the
// formats Claude and OpenCode both take as vision input. A closed set
// (not a prefix check) stops the presign endpoint from minting upload
// slots for arbitrary content.
var AllowedMimes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// Config wires a Server.
type Config struct {
	// Storage is the object store images live in — its own dedicated
	// bucket. Required.
	Storage blobstore.Storage

	// PresignTTL overrides DefaultPresignTTL when non-zero.
	PresignTTL time.Duration
}

// Server mints presigned image upload/download URLs.
type Server struct {
	store         blobstore.Storage
	presignTTL    time.Duration
	entropySource io.Reader // cryptorand.Reader by default; injectable for tests
}

// NewServer constructs a Server. Storage is required — fail fast at
// startup rather than 500 per request.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Storage == nil {
		return nil, errors.New("images: Config.Storage is required")
	}
	ttl := cfg.PresignTTL
	if ttl == 0 {
		ttl = DefaultPresignTTL
	}
	return &Server{store: cfg.Storage, presignTTL: ttl, entropySource: cryptorand.Reader}, nil
}

// Handler returns the image routes. Mount under the gateway's
// authenticated surface — the handler reads the caller from the
// auth.Principal the outer middleware injects.
func (s *Server) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("POST /v1/images", s.handlePresignImage)
	return mx
}
