package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Pusher is the laptop-side HTTP client that posts bundles to the
// remote hub's POST /sync/repos/{key}/bundle endpoint. It is
// stateless — one Pusher instance can service many concurrent pushes.
type Pusher struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewPusher constructs a Pusher targeting baseURL (e.g.
// "https://cloud-hub.example") with the shared bearer token. Pass nil
// for httpClient to use a default with a generous timeout.
func NewPusher(baseURL, token string, httpClient *http.Client) *Pusher {
	if httpClient == nil {
		// No overall request timeout — bundles for large repos can take
		// a while to upload, and we control both sides. Caller passes
		// per-call deadlines via context instead.
		httpClient = &http.Client{}
	}
	return &Pusher{baseURL: baseURL, token: token, client: httpClient}
}

// PushRequest carries everything needed to ship one branch's bundle.
type PushRequest struct {
	RepoKey   string
	RemoteURL string
	Branch    string
	TipSHA    string
	BaseSHA   string
	Bundle    io.Reader
}

// Push streams the bundle to the remote hub. Returns nil on 204.
func (p *Pusher) Push(ctx context.Context, req PushRequest) error {
	if p.baseURL == "" {
		return fmt.Errorf("pusher: base URL is required")
	}
	url := p.baseURL + "/sync/repos/" + req.RepoKey + "/bundle"
	r, err := http.NewRequestWithContext(ctx, "POST", url, req.Bundle)
	if err != nil {
		return fmt.Errorf("build sync request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+p.token)
	r.Header.Set("Content-Type", "application/x-git-bundle")
	r.Header.Set("X-Clank-Branch", req.Branch)
	r.Header.Set("X-Clank-Remote-URL", req.RemoteURL)
	if req.TipSHA != "" {
		r.Header.Set("X-Clank-Tip-SHA", req.TipSHA)
	}
	if req.BaseSHA != "" {
		r.Header.Set("X-Clank-Base-SHA", req.BaseSHA)
	}

	resp, err := p.client.Do(r)
	if err != nil {
		return fmt.Errorf("sync push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("sync push: %s: %s", resp.Status, body)
}
