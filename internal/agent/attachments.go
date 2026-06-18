package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/acksell/clank/pkg/images"
)

// maxImageBytes caps a single downloaded attachment. Anthropic rejects
// images over ~5 MB, so we fail fast rather than stream an oversized blob
// into the agent.
const maxImageBytes = 5 << 20 // 5 MiB

// imageHTTPClient downloads attachments. A standalone client with a
// timeout so a hung object store can't wedge a send indefinitely; the
// caller's context still bounds cancellation.
var imageHTTPClient = &http.Client{Timeout: 60 * time.Second}

// resolvedImage is an attachment whose bytes have been downloaded and
// validated, ready to inline into an agent message.
type resolvedImage struct {
	Mime     string
	Filename string
	Data     []byte
}

// resolveAttachments downloads every attachment via its presigned GetURL.
// It fails fast on the first error — a missing/oversized/wrong-type image
// must surface, never be silently dropped (a half-sent message is worse
// than a rejected one).
func resolveAttachments(ctx context.Context, atts []Attachment) ([]resolvedImage, error) {
	out := make([]resolvedImage, 0, len(atts))
	for _, a := range atts {
		data, err := resolveImage(ctx, a)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", a.ImageID, err)
		}
		out = append(out, resolvedImage{Mime: a.Mime, Filename: a.Filename, Data: data})
	}
	return out, nil
}

// resolveImage downloads one attachment's bytes from its presigned GET
// URL, enforcing the mime allowlist and a size cap. GetURL is required —
// no fallback to the object store (the sprite holds no credentials).
func resolveImage(ctx context.Context, a Attachment) ([]byte, error) {
	if a.GetURL == "" {
		return nil, fmt.Errorf("missing get_url")
	}
	if !images.AllowedMimes[a.Mime] {
		return nil, fmt.Errorf("unsupported image mime %q", a.Mime)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.GetURL, nil)
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
	if len(data) == 0 {
		return nil, fmt.Errorf("empty image body")
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", maxImageBytes)
	}
	return data, nil
}

// dataURL renders bytes as an RFC 2397 data: URL — OpenCode's file part
// accepts this inline form, symmetric with Claude's base64 image block.
func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
