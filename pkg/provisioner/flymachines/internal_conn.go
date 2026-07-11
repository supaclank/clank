// GetHostByID + OpenInternalConn capability extensions.
//
// GetHostByID is a pure store read building the same HostRef shape
// EnsureHost returns — the preview-route proxy resolves a token's
// target without waking anything.
//
// OpenInternalConn tunnels through clank-host's own /tunnel/{port}
// endpoint (pkg/provisioner/tunnelclient): Flycast routes only the
// one declared service port, so arbitrary preview-app ports ride
// inside it. The Flycast dial autostarts a stopped machine, matching
// the sprites property preview traffic relies on.
package flymachines

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	transportpkg "github.com/acksell/clank/pkg/provisioner/transport"
	"github.com/acksell/clank/pkg/provisioner/tunnelclient"
)

// GetHostByID implements provisioner.Provisioner. Strictly a read —
// never provisions, wakes, or creates.
func (p *Provisioner) GetHostByID(ctx context.Context, hostID string) (provisioner.HostRef, error) {
	if hostID == "" {
		return provisioner.HostRef{}, fmt.Errorf("flymachines: hostID is required")
	}
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		if errors.Is(err, hoststore.ErrHostNotFound) {
			return provisioner.HostRef{}, err
		}
		return provisioner.HostRef{}, fmt.Errorf("flymachines: store get-by-id: %w", err)
	}
	if row.LastURL == "" {
		return provisioner.HostRef{}, fmt.Errorf("flymachines: host %s has no last_url (corrupt row)", hostID)
	}
	parsedURL, err := url.Parse(row.LastURL)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("flymachines: parse host URL %q: %w", row.LastURL, err)
	}
	return provisioner.HostRef{
		HostID:    row.ID,
		URL:       row.LastURL,
		Transport: &transportpkg.BearerInjector{Token: row.AuthToken, Host: parsedURL.Host},
		AuthToken: row.AuthToken,
		AutoWake:  true,
		Hostname:  row.Hostname,
	}, nil
}

// OpenInternalConn implements provisioner.Provisioner via the
// clank-host tunnel endpoint. The returned conn is single-shot; the
// caller (previewtunnel's http.Transport) handles pooling.
func (p *Provisioner) OpenInternalConn(ctx context.Context, hostID string, port int) (net.Conn, error) {
	if hostID == "" {
		return nil, fmt.Errorf("flymachines: hostID is required")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("flymachines: port %d out of range", port)
	}
	ref, err := p.GetHostByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	conn, err := tunnelclient.Dial(ctx, ref.URL, ref.Transport, port)
	if err != nil {
		return nil, fmt.Errorf("flymachines: tunnel to %s port %d: %w", ref.Hostname, port, err)
	}
	return conn, nil
}
