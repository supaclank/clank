// Local-subprocess implementation of GetHostByID + OpenInternalConn.
//
// In local mode the daemon and the spawned clank-host (and any dev
// servers it forks) all run on the same machine. The "internal"
// network is just loopback — port `port` lives at 127.0.0.1:<port>
// regardless of what URL clank-host's HTTP listener is on.
//
// Note that c.url is clank-host's own listener URL (the HTTP control
// plane), not the per-worktree dev-server port the caller wants. The
// dev server (Metro, etc.) binds an OS-allocated port that nobody
// outside the preview manager knows in advance — the caller passes it
// in via the `port` arg here. So even though we have a URL on the
// child record, we ignore its host and dial 127.0.0.1 explicitly.
//
// This also makes the local provisioner a useful test substrate for
// previewtunnel.RoundTripper: stand up an httptest server, dial
// through OpenInternalConn, and you've exercised the full
// Provisioner.OpenInternalConn → http.Transport → upstream chain
// without needing a real cloud provider.
package local

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	transportpkg "github.com/acksell/clank/pkg/provisioner/transport"
)

const internalDialTimeout = 5 * time.Second

// GetHostByID returns the cached HostRef when the running child's
// hostID matches; otherwise reports the host as missing.
//
// The local provisioner only ever holds one child (one user), so any
// non-matching hostID is by definition not on this host. Mirrors the
// EnsureHost contract: we don't spawn a new child on a lookup.
func (p *Provisioner) GetHostByID(_ context.Context, hostID string) (provisioner.HostRef, error) {
	if hostID == "" {
		return provisioner.HostRef{}, fmt.Errorf("local provisioner: hostID is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.hostID != hostID {
		return provisioner.HostRef{}, hoststore.ErrHostNotFound
	}
	return p.refFromChildLocked(p.current), nil
}

// OpenInternalConn dials 127.0.0.1:<port> directly. hostID must match
// the currently-running child so we don't paper over a stale ref
// pointing at a different machine state.
func (p *Provisioner) OpenInternalConn(ctx context.Context, hostID string, port int) (net.Conn, error) {
	if hostID == "" {
		return nil, fmt.Errorf("local provisioner: hostID is required")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("local provisioner: port %d out of range", port)
	}
	p.mu.Lock()
	matches := p.current != nil && p.current.hostID == hostID
	p.mu.Unlock()
	if !matches {
		return nil, hoststore.ErrHostNotFound
	}
	d := net.Dialer{Timeout: internalDialTimeout}
	return d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// GetHostByNotifierToken implements gateway.PreviewHostLookup (and
// notify.HostLookup): given a notifier_token bearer presented by a
// webhook caller, return the matching host row.
//
// In local mode the row isn't in Postgres — it's in p.current. The
// preview webhook flow (clank-host → POST /webhooks/preview/register)
// uses this lookup to authenticate the inbound bearer.
func (p *Provisioner) GetHostByNotifierToken(_ context.Context, notifierToken string) (hoststore.Host, error) {
	if notifierToken == "" {
		return hoststore.Host{}, hoststore.ErrHostNotFound
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.notifierToken != notifierToken {
		return hoststore.Host{}, hoststore.ErrHostNotFound
	}
	c := p.current
	return hoststore.Host{
		ID:            c.hostID,
		UserID:        "local",
		Provider:      "local",
		Hostname:      c.hostname,
		AuthToken:     c.authToken,
		NotifierToken: c.notifierToken,
		Status:        hoststore.HostStatusRunning,
	}, nil
}

// refFromChildLocked rebuilds a HostRef from a *child. Caller holds
// p.mu. Symmetric with the EnsureHost path's construction so two
// callers comparing HostRefs see consistent shapes (only Transport
// will differ — it's a fresh struct per call here vs. cached
// per-child elsewhere; both wire the same per-host bearer).
func (p *Provisioner) refFromChildLocked(c *child) provisioner.HostRef {
	return provisioner.HostRef{
		HostID:    c.hostID,
		URL:       c.url,
		Transport: &transportpkg.BearerInjector{Token: c.authToken},
		AuthToken: c.authToken,
		AutoWake:  false, // no edge wake for a local subprocess
		Hostname:  c.hostname,
	}
}
