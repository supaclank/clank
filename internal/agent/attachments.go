package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/supaclank/clank/pkg/images"
)

// maxImageBytes caps a single attachment, matching the gateway's intent
// (Anthropic ~5 MB) so we reject early rather than blow up the agent payload.
const maxImageBytes = 5 << 20 // 5 MiB

// AllowLocalFileAttachments gates file:// attachment sources. clank-host enables
// it only in laptop (socket) mode, where the client shares its filesystem; a
// remote sprite leaves it false so a message can't make it read arbitrary local
// paths.
var AllowLocalFileAttachments bool

// imageHTTPClient downloads http(s) attachments. Standalone with a timeout so a
// hung object store can't wedge a send; the caller's context still cancels.
var imageHTTPClient = &http.Client{Timeout: 60 * time.Second}

// resolvedImage is an attachment whose bytes are in hand, ready to inline.
type resolvedImage struct {
	Mime     string
	Filename string
	Data     []byte
}

// ResolvedImage aliases resolvedImage for backend subpackages
// (internal/agent/acp); the fields are already exported.
type ResolvedImage = resolvedImage

// ResolveAttachments is the exported entry to resolveAttachments for
// backend subpackages.
func ResolveAttachments(ctx context.Context, atts []Attachment) ([]ResolvedImage, error) {
	return resolveAttachments(ctx, atts)
}

// resolveAttachments resolves every attachment's bytes. Fails fast on the first
// error — a missing/oversized/wrong-type image must surface, never be silently
// dropped.
// TODO(ai-review): download attachments concurrently (errgroup + order preservation) to reduce latency for multi-image messages https://github.com/supaclank/clank/pull/64#discussion_r3438430026
func resolveAttachments(ctx context.Context, atts []Attachment) ([]resolvedImage, error) {
	out := make([]resolvedImage, 0, len(atts))
	for _, a := range atts {
		data, err := resolveImage(ctx, a)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", a.Filename, err)
		}
		out = append(out, resolvedImage{Mime: a.Mime, Filename: a.Filename, Data: data})
	}
	return out, nil
}

// resolveImage fetches an attachment's bytes from its Source, dispatching on
// scheme: data: (inline base64), file:// (local read, gated), or http(s):// (a
// fetchable URL such as a presigned object-store GET). The declared mime must be
// allowed and the payload within maxImageBytes.
// TODO(ai-review): validate Source origin against configured image-store hostname(s) before issuing the request to prevent SSRF from authenticated clients https://github.com/supaclank/clank/pull/64#discussion_r3438446244
func resolveImage(ctx context.Context, a Attachment) ([]byte, error) {
	if !images.AllowedMimes[a.Mime] {
		return nil, fmt.Errorf("unsupported image mime %q", a.Mime)
	}
	switch {
	case a.Source == "":
		return nil, fmt.Errorf("missing attachment source")
	case strings.HasPrefix(a.Source, "data:"):
		return decodeDataURL(a.Source)
	case strings.HasPrefix(a.Source, "file://"):
		if !AllowLocalFileAttachments {
			return nil, fmt.Errorf("file:// attachments are not allowed on this host")
		}
		return readLocalImage(a.Source)
	case strings.HasPrefix(a.Source, "http://"), strings.HasPrefix(a.Source, "https://"):
		return downloadImage(ctx, a.Source)
	default:
		return nil, fmt.Errorf("unsupported attachment source scheme")
	}
}

func decodeDataURL(src string) ([]byte, error) {
	comma := strings.IndexByte(src, ',')
	if comma < 0 || !strings.Contains(src[:comma], ";base64") {
		return nil, fmt.Errorf("malformed data URL")
	}
	data, err := base64.StdEncoding.DecodeString(src[comma+1:])
	if err != nil {
		return nil, fmt.Errorf("decode data URL: %w", err)
	}
	return capImage(data)
}

func readLocalImage(src string) ([]byte, error) {
	path := strings.TrimPrefix(src, "file://")
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("file:// source must be an absolute path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat image: %w", err)
	}
	if info.Size() > maxImageBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", maxImageBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	return capImage(data)
}

func downloadImage(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := imageHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download image: status %d", resp.StatusCode)
	}
	// Read one byte past the cap so an over-limit blob is detectable.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	return capImage(data)
}

func capImage(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty image body")
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", maxImageBytes)
	}
	return data, nil
}

// DataURL renders bytes as an RFC 2397 data: URL — used to inline an image into
// an OpenCode file part and to build an inline attachment Source.
func DataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
