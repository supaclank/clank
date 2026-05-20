// Package sprites implements a keepalive.Listener backed by the Fly
// Sprites Tasks API (https://docs.sprites.dev/keeping-sprites-running/).
//
// While the agent is active the Listener PUTs a self-expiring task
// over the local management socket; while present, the task prevents
// the sprite's last-consumer timer from hibernating the VM. The 5-min
// lease is the cost safety — a crashed clank-host can't keep the VM
// alive longer than the lease, regardless of pending work.
package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

const (
	// socketPath is the unix-domain socket the Sprites runtime exposes
	// inside every VM for management calls.
	socketPath = "/.sprite/api.sock"

	// tasksBasePath + "/" + leaseName is the lease URL.
	tasksBasePath = "/v1/tasks"

	// leaseName identifies the lease we hold. Sprites tasks are keyed
	// by name; reusing the same name across PUTs renews rather than
	// creating duplicates.
	leaseName = "agents"

	// leaseExpire is the Sprites-side TTL of each PUT. Sprites docs
	// recommend a 5-minute expiry refreshed every minute; we refresh
	// every keepalive.DefaultInterval (~30s) for safety.
	leaseExpire = "5m"

	// idleThreshold gates whether a Tick renews the lease. If no Bump
	// has happened within this window, Tick is a no-op and the lease
	// expires naturally. Tunes how long after the last event a sprite
	// stays running before hibernating.
	idleThreshold = 3 * time.Minute
)

// Listener PUTs /v1/tasks/agents while activity is recent.
type Listener struct {
	http *http.Client
	log  *log.Logger
}

// New constructs a Listener wired to the local Sprites management
// socket. Passing a nil logger uses log.Default().
func New(lg *log.Logger) *Listener {
	return newWithClient(newSocketClient(socketPath), lg)
}

// newWithClient is the test seam — construct the Listener with a
// caller-supplied http.Client (e.g. one whose Transport dials a
// tmp socket bound to httptest.Server).
func newWithClient(c *http.Client, lg *log.Logger) *Listener {
	if lg == nil {
		lg = log.Default()
	}
	return &Listener{log: lg, http: c}
}

// Tick PUTs the lease when lastActivity is recent. No-op on the zero
// time (no Bump yet) or when activity is older than idleThreshold.
func (l *Listener) Tick(ctx context.Context, lastActivity time.Time) {
	if lastActivity.IsZero() || time.Since(lastActivity) >= idleThreshold {
		return
	}
	if err := l.renew(ctx); err != nil {
		l.log.Printf("sprites keepalive: renew lease: %v", err)
	}
}

// Close issues a best-effort DELETE so the sprite hibernates promptly
// on graceful shutdown rather than waiting for leaseExpire.
func (l *Listener) Close(ctx context.Context) error {
	return l.release(ctx)
}

func (l *Listener) renew(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"expire": leaseExpire})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, leaseURL(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.http.Do(req)
	if err != nil {
		return fmt.Errorf("do PUT: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT %s: status %d body=%q", leaseURL(), resp.StatusCode, snippet)
	}
	return nil
}

func (l *Listener) release(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, leaseURL(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return fmt.Errorf("do DELETE: %w", err)
	}
	defer resp.Body.Close()
	// 404 is fine — lease already expired or never created.
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("DELETE %s: status %d body=%q", leaseURL(), resp.StatusCode, snippet)
	}
	return nil
}

func leaseURL() string {
	// Sprites resolves the Host header internally; the "sprite" name is
	// the conventional placeholder used across the docs.
	return "http://sprite" + tasksBasePath + "/" + leaseName
}

// newSocketClient builds an http.Client whose Transport dials the given
// unix-domain socket regardless of the request's URL.
func newSocketClient(path string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}
