package daemonclient

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// PreviewSignedURL is a short-lived owner-only browser URL.
type PreviewSignedURL struct {
	SignedURL string    `json:"signed_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SignPreviewToken authorizes a browser that cannot attach the CLI's bearer token.
func (c *Client) SignPreviewToken(ctx context.Context, token string, ttl time.Duration) (PreviewSignedURL, error) {
	if token == "" {
		return PreviewSignedURL{}, fmt.Errorf("preview token is required")
	}
	if ttl <= 0 {
		return PreviewSignedURL{}, fmt.Errorf("preview signed URL TTL must be positive")
	}
	var out PreviewSignedURL
	path := "/v1/preview/tokens/" + url.PathEscape(token) + "/sign"
	body := struct {
		TTL string `json:"ttl"`
	}{TTL: ttl.String()}
	if err := c.post(ctx, path, body, &out); err != nil {
		return PreviewSignedURL{}, err
	}
	return out, nil
}
