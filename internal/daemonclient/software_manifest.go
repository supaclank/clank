package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// softwareManifestRetries is the number of attempts (including the
// first) when fetching the remote's software manifest. The retry
// covers cold-starting sprites: the gateway returns 502 "host
// unavailable" while the sprite VM is waking up, and a short backoff
// is usually enough to land on a warm sprite.
const softwareManifestRetries = 3

// softwareManifestBackoffBaseForTest is the initial sleep between
// retries. Doubles on each attempt (1s, 2s, 4s). Held as a var so
// the test suite can shrink it to keep CI fast; production callers
// should never mutate.
var softwareManifestBackoffBaseForTest = time.Second

// SoftwareManifest calls clank-host's GET /software-manifest
// (proxied through whichever daemon this client targets) and
// returns the manifest of versions for every relevant CLI tool
// installed on that host. Today only opencode is populated; the
// shape is forward-compatible for claude / clank-host / etc.
//
// Retries up to softwareManifestRetries times with exponential
// backoff when the underlying call fails with a gateway-side
// 5xx that looks like a cold sprite (502/503/504 / "host
// unavailable"). Other errors propagate immediately.
//
// Cached aggressively on the server side, so this is effectively
// free after the first invocation per clank-host process lifetime.
// See agent.GetSoftwareManifest's docstring for the freshness
// contract.
func (c *Client) SoftwareManifest(ctx context.Context) (agent.SoftwareManifest, error) {
	var out agent.SoftwareManifest
	var lastErr error
	for attempt := 0; attempt < softwareManifestRetries; attempt++ {
		err := c.get(ctx, "/software-manifest", &out)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !isColdSpriteError(err) {
			return agent.SoftwareManifest{}, fmt.Errorf("software-manifest: %w", err)
		}
		if attempt == softwareManifestRetries-1 {
			break
		}
		wait := softwareManifestBackoffBaseForTest << attempt // 1s, 2s, 4s in production
		select {
		case <-ctx.Done():
			return agent.SoftwareManifest{}, fmt.Errorf("software-manifest: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
	return agent.SoftwareManifest{}, fmt.Errorf("software-manifest: %w", lastErr)
}

// isColdSpriteError detects errors that plausibly come from a
// cold-starting sprite VM behind the gateway. String-matched
// because do() folds the status code into a stringly-typed error.
// Conservative: only retries on these specific markers — any other
// error propagates immediately so genuine misconfigurations don't
// hide behind backoff.
func isColdSpriteError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	for _, marker := range []string{"status 502", "status 503", "status 504", "host unavailable"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
