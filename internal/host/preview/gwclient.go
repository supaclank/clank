// GWClient is the sprite-side HTTP client for the gateway's
// /webhooks/preview/{register,revoke} endpoints. Manager calls
// Register after Metro reaches Ready and Revoke on Stop/Shutdown
// /reap so the gateway's preview_routes table reflects sprite
// reality.
//
// Bearer auth: the gateway authenticates via the per-host
// notifier_token (same one used for /webhooks/notifications). One
// credential, two webhooks.
//
// Lifecycle:
//   - Construct via NewGWClient(baseURL, bearer). Empty baseURL =
//     "no gateway wired" (laptop dev path); methods become no-ops
//     that succeed without making HTTP calls.
//   - All methods take a context for cancellation. They surface HTTP
//     transport / status errors directly; Manager logs and continues
//     rather than blocking start/stop on webhook flakiness.
package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/supaclank/clank/pkg/preview/tokens"
)

const (
	registerPath = "/register"
	revokePath   = "/revoke"

	gwClientTimeout = 10 * time.Second
)

// GWClient is the gateway webhook client. Nil-safe: a nil receiver
// makes every method a no-op success. Used in tests + laptop dev.
type GWClient struct {
	baseURL string // e.g. "https://api.example.dev/webhooks/preview"
	bearer  string
	http    *http.Client
}

// NewGWClient builds a GWClient. Pass empty baseURL to disable the
// integration entirely (no webhook calls; Register returns a zero
// RegisterResponse). bearer is the per-host notifier_token.
func NewGWClient(baseURL, bearer string) *GWClient {
	baseURL = strings.TrimRight(baseURL, "/")
	return &GWClient{
		baseURL: baseURL,
		bearer:  bearer,
		http:    &http.Client{Timeout: gwClientTimeout},
	}
}

// Enabled reports whether the client will actually make HTTP calls.
// Lets Manager log a "running without gateway integration" warning
// on cold start without touching the no-op control flow.
func (c *GWClient) Enabled() bool {
	return c != nil && c.baseURL != "" && c.bearer != ""
}

// RegisterRequest mirrors gateway/webhook_preview.go's
// previewRegisterRequest. Kept here as its own type so the sprite
// doesn't depend on the gateway package — the two contracts agree
// via JSON tags, not Go-level type sharing.
type RegisterRequest struct {
	WorktreeID   string `json:"worktree_id"`
	ServiceName  string `json:"service_name"`
	InternalPort int    `json:"internal_port"`
}

// RegisterResponse is what the gateway returns. visibility starts at
// owner_only on first register; mobile/owner flips it later via the
// owner-facing /v1/preview/tokens/{token}/share endpoint.
type RegisterResponse struct {
	Token      string            `json:"token"`
	URL        string            `json:"url"`
	Visibility tokens.Visibility `json:"visibility"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

// Register calls /webhooks/preview/register. When the client is
// disabled (no baseURL), returns a zero RegisterResponse and nil
// error — Manager treats this as "preview is running but no public
// URL was minted" so callers can still introspect Status.
func (c *GWClient) Register(ctx context.Context, req RegisterRequest) (RegisterResponse, error) {
	if !c.Enabled() {
		return RegisterResponse{}, nil
	}
	body, err := json.Marshal(req)
	if err != nil {
		return RegisterResponse{}, fmt.Errorf("preview gwclient: marshal register: %w", err)
	}
	resp, err := c.do(ctx, "POST", registerPath, body)
	if err != nil {
		return RegisterResponse{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return RegisterResponse{}, fmt.Errorf("preview gwclient: read register response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return RegisterResponse{}, fmt.Errorf("preview gwclient: register status %d: %s", resp.StatusCode, snippet(respBody))
	}
	var out RegisterResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return RegisterResponse{}, fmt.Errorf("preview gwclient: decode register response: %w", err)
	}
	return out, nil
}

// RevokeRequest is the body for /webhooks/preview/revoke.
type RevokeRequest struct {
	WorktreeID  string `json:"worktree_id"`
	ServiceName string `json:"service_name"`
}

// Revoke calls /webhooks/preview/revoke. Best-effort: gateway also
// idempotently no-ops on unknown (host, wid, svc) triples, so a
// duplicate Revoke from a flaky network won't surface as an error.
func (c *GWClient) Revoke(ctx context.Context, req RevokeRequest) error {
	if !c.Enabled() {
		return nil
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("preview gwclient: marshal revoke: %w", err)
	}
	resp, err := c.do(ctx, "POST", revokePath, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 204 is the documented success code; any 2xx is fine.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4*1024))
	return fmt.Errorf("preview gwclient: revoke status %d: %s", resp.StatusCode, snippet(body))
}

func (c *GWClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("preview gwclient: build %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("preview gwclient: %s %s: %w", method, path, err)
	}
	return resp, nil
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
