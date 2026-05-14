package daemonclient

import (
	"context"
	"fmt"

	"github.com/acksell/clank/internal/agent"
)

// SoftwareManifest calls clank-host's GET /software-manifest
// (proxied through whichever daemon this client targets) and
// returns the manifest of versions for every relevant CLI tool
// installed on that host. Today only opencode is populated; the
// shape is forward-compatible for claude / clank-host / etc.
//
// Cached aggressively on the server side, so this is effectively
// free after the first invocation per clank-host process lifetime.
// See agent.GetSoftwareManifest's docstring for the freshness
// contract.
func (c *Client) SoftwareManifest(ctx context.Context) (agent.SoftwareManifest, error) {
	var out agent.SoftwareManifest
	if err := c.get(ctx, "/software-manifest", &out); err != nil {
		return agent.SoftwareManifest{}, fmt.Errorf("software-manifest: %w", err)
	}
	return out, nil
}
