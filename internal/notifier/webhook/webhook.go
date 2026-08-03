// Package webhook implements notifier.Provider as an HTTP POST to a
// configured URL. The body is the JSON-encoded notifier.Notification;
// when Token is set, the request carries "Authorization: Bearer
// <token>" so the receiver can identify the calling host. Format and
// rotation of the token live entirely on the receiver side — this
// Provider treats it as an opaque string.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/supaclank/clank/internal/notifier"
)

const (
	// requestTimeout is the per-attempt HTTP timeout. The Loop also
	// applies a higher-level SendTimeout that includes retries; this
	// bound keeps a single hung attempt from consuming the entire
	// budget.
	requestTimeout = 3 * time.Second

	// retryDelay is the pause between the first attempt and the single
	// retry on 5xx / network error.
	retryDelay = 1 * time.Second
)

// Provider POSTs Notifications to URL. Construct with New.
type Provider struct {
	url   string
	token string
	http  *http.Client
	log   *log.Logger
}

// New constructs a Provider. A nil logger uses a stderr-prefixed
// default. Empty token sends unauthenticated requests — fine for dev
// against a local sink, never for production.
func New(url, token string, lg *log.Logger) *Provider {
	if lg == nil {
		lg = log.New(os.Stderr, "[notifier-webhook] ", log.LstdFlags|log.Lmsgprefix)
	}
	return &Provider{
		url:   url,
		token: token,
		http:  &http.Client{Timeout: requestTimeout},
		log:   lg,
	}
}

// Send POSTs n as JSON. On 5xx or network error it retries once after
// retryDelay; 4xx is non-retryable and returns immediately. No DLQ —
// the Loop owns the lifecycle and we don't try to outsmart it.
func (p *Provider) Send(ctx context.Context, n notifier.Notification) error {
	body, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	attempt := func() (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
		if err != nil {
			return 0, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if p.token != "" {
			req.Header.Set("Authorization", "Bearer "+p.token)
		}
		resp, err := p.http.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return resp.StatusCode, fmt.Errorf("POST %s: status %d body=%q", p.url, resp.StatusCode, snippet)
		}
		return resp.StatusCode, nil
	}

	status, err := attempt()
	if err == nil {
		return nil
	}
	// 4xx: client error; retrying won't help.
	if status >= 400 && status < 500 {
		return err
	}
	p.log.Printf("retry after %v: %v", retryDelay, err)
	select {
	case <-time.After(retryDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	_, retryErr := attempt()
	return retryErr
}

// Close is a no-op — net/http connection pooling cleans up
// automatically when the client is garbage-collected.
func (p *Provider) Close(_ context.Context) error { return nil }
