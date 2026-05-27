// Daytona implementation of GetHostByID + OpenInternalConn.
//
// Daytona's preview proxy does support WebSocket upgrade (per
// https://www.daytona.io/docs/en/custom-preview-proxy/#websocket-support),
// so the HMR-blocking concern doesn't apply. The interface mismatch
// is what blocks today's OpenInternalConn: Daytona exposes preview
// access via a per-port HTTPS URL (sandbox.getPreviewLink() /
// getSignedPreviewUrl()), not a net.Conn-style TCP tunnel.
//
// To support preview-app on Daytona we'd add a sibling capability —
// something like `Provisioner.PublicPreviewURL(ctx, host, port,
// opts) (string, error)` — that the gateway preview proxy
// dispatches to in addition to OpenInternalConn. The two transports
// can coexist behind the same subdomain-routing surface.
//
// Until that lands, OpenInternalConn returns ErrUnsupported on
// Daytona and the feature gracefully degrades (gateway surfaces 503).
//
// GetHostByID still works: it's a store-only lookup that rebuilds the
// same HostRef shape EnsureHost returns, useful for any callers that
// need the host's URL/transport without going through provisioning.
package daytona

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
)

// GetHostByID looks up a sandbox by stored host_id and builds a
// HostRef pointing at it. Read-only — does not provision or warm.
func (p *Provisioner) GetHostByID(ctx context.Context, hostID string) (provisioner.HostRef, error) {
	if hostID == "" {
		return provisioner.HostRef{}, fmt.Errorf("daytona provisioner: hostID is required")
	}
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		if errors.Is(err, hoststore.ErrHostNotFound) {
			return provisioner.HostRef{}, err
		}
		return provisioner.HostRef{}, fmt.Errorf("daytona provisioner: store get-by-id: %w", err)
	}
	if row.LastURL == "" {
		return provisioner.HostRef{}, fmt.Errorf("daytona provisioner: host %s has no last_url (corrupt row)", hostID)
	}
	transport, err := chainTransport(row.AuthToken, row.LastToken, row.LastURL)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("daytona provisioner: build transport: %w", err)
	}
	return provisioner.HostRef{
		HostID:    row.ID,
		URL:       row.LastURL,
		Transport: transport,
		AuthToken: row.AuthToken,
		AutoWake:  false, // Daytona doesn't auto-wake on edge traffic
		Hostname:  row.Hostname,
	}, nil
}

// OpenInternalConn is not implemented for Daytona — the platform
// doesn't expose a TCP-tunnel primitive analogous to Sprites' WSS
// proxy. Preview-app traffic (which is the only caller today) will
// see ErrUnsupported and the gateway returns 503.
func (p *Provisioner) OpenInternalConn(_ context.Context, _ string, _ int) (net.Conn, error) {
	return nil, fmt.Errorf("daytona: %w", provisioner.ErrUnsupported)
}
