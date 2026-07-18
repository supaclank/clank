package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/acksell/clank/internal/lannet"
)

// DefaultPort is the bridge listener's fixed port — the phone's stored
// gateway URLs must survive daemon restarts. 7879 belongs to
// clank-auth-stub; stay clear.
const DefaultPort = 7880

// bindReason explains a bind in status output.
type bindReason string

const (
	bindLoopback bindReason = "loopback"
	bindTailnet  bindReason = "tailnet"
	bindLAN      bindReason = "lan"
)

// desiredBind is one address the policy wants a listener on.
type desiredBind struct {
	IP     string
	Reason bindReason
}

// desiredBinds is the whole bind policy, pure for testability:
// loopback always; tailnet whenever present (WireGuard-encrypted,
// peer-authed); the physical LAN only on a network the user has
// consented to ("trust this LAN?"). A CGNAT lanIP is the tailnet
// address seen through the default route (exit-node case) — never a
// physical LAN.
func desiredBinds(store *Store, tn *Tailnet, lanIP net.IP, network Network) []desiredBind {
	out := []desiredBind{{IP: "127.0.0.1", Reason: bindLoopback}}
	if tn != nil {
		out = append(out, desiredBind{IP: tn.IP, Reason: bindTailnet})
	}
	if lanIP != nil && !lannet.IsCGNAT(lanIP) && store.NetworkTrusted(network.Fingerprint) {
		out = append(out, desiredBind{IP: lanIP.String(), Reason: bindLAN})
	}
	return out
}

// BindStatus reports one address the last Refresh wanted, and how the
// bind went ("" = serving).
type BindStatus struct {
	IP     string     `json:"ip"`
	Reason bindReason `json:"reason"`
	Err    string     `json:"err,omitempty"`
}

// Status is the transport snapshot the admin surface exposes.
type Status struct {
	Port    int          `json:"port"`
	Binds   []BindStatus `json:"binds"`
	Tailnet *Tailnet     `json:"tailnet,omitempty"`
	LANIP   string       `json:"lan_ip,omitempty"`
	Network Network      `json:"network"`
	// NetworkTrusted mirrors store consent for the CURRENT network so
	// the CLI can decide whether to prompt.
	NetworkTrusted bool `json:"network_trusted"`
}

// ListenerOptions configures Listeners. The discovery funcs default to
// the real implementations; tests inject fakes.
type ListenerOptions struct {
	Port    int
	Handler http.Handler
	Store   *Store
	Log     *log.Logger

	LANIP   func() (net.IP, error)
	Tailnet func(context.Context) *Tailnet
	Network func(context.Context) Network
}

// Listeners owns one http.Server per bound address, sharing the bridge
// handler. Refresh reconciles the running set against the policy; it
// runs on demand (daemon start, admin calls, preview start), not on a
// timer.
type Listeners struct {
	opts ListenerOptions

	mu      sync.Mutex
	servers map[string]*boundServer
	status  Status
}

type boundServer struct {
	srv *http.Server
	ln  net.Listener
}

// NewListeners builds the manager; call Refresh to bind.
func NewListeners(opts ListenerOptions) *Listeners {
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	if opts.Log == nil {
		opts.Log = log.Default()
	}
	if opts.LANIP == nil {
		opts.LANIP = lannet.LANIP
	}
	if opts.Tailnet == nil {
		opts.Tailnet = DiscoverTailnet
	}
	if opts.Network == nil {
		opts.Network = CurrentNetwork
	}
	return &Listeners{opts: opts, servers: make(map[string]*boundServer)}
}

// Refresh re-runs discovery, reconciles listeners to the policy, and
// returns the resulting snapshot. Bind failures are recorded, never
// fatal — the daemon runs bridgeless rather than dying.
func (l *Listeners) Refresh(ctx context.Context) Status {
	tn := l.opts.Tailnet(ctx)
	network := l.opts.Network(ctx)
	var lanIP net.IP
	if ip, err := l.opts.LANIP(); err == nil {
		lanIP = ip
	}
	desired := desiredBinds(l.opts.Store, tn, lanIP, network)

	l.mu.Lock()
	defer l.mu.Unlock()

	want := make(map[string]bool, len(desired))
	for _, d := range desired {
		want[d.IP] = true
	}
	// Stop binds the policy no longer wants (network change, trust
	// revoked by hand-editing bridge.json).
	for ip, bs := range l.servers {
		if want[ip] {
			continue
		}
		l.shutdownLocked(bs)
		delete(l.servers, ip)
		l.opts.Log.Printf("bridge: unbound %s:%d", ip, l.opts.Port)
	}

	status := Status{
		Port:           l.opts.Port,
		Tailnet:        tn,
		Network:        network,
		NetworkTrusted: l.opts.Store.NetworkTrusted(network.Fingerprint),
	}
	if lanIP != nil {
		status.LANIP = lanIP.String()
	}
	for _, d := range desired {
		bs := BindStatus{IP: d.IP, Reason: d.Reason}
		if _, up := l.servers[d.IP]; !up {
			if err := l.bindLocked(d.IP); err != nil {
				bs.Err = err.Error()
			} else {
				l.opts.Log.Printf("bridge: listening on %s:%d (%s)", d.IP, l.opts.Port, d.Reason)
			}
		}
		status.Binds = append(status.Binds, bs)
	}
	l.status = status
	return status
}

// LastStatus returns the snapshot from the most recent Refresh.
func (l *Listeners) LastStatus() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// Close stops every listener.
func (l *Listeners) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, bs := range l.servers {
		l.shutdownLocked(bs)
		delete(l.servers, ip)
	}
}

func (l *Listeners) bindLocked(ip string) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, fmt.Sprintf("%d", l.opts.Port)))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           l.opts.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE (/events) streams indefinitely, same
		// posture as the old front door.
	}
	l.servers[ip] = &boundServer{srv: srv, ln: ln}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.opts.Log.Printf("bridge: serve %s: %v", ip, err)
		}
	}()
	return nil
}

func (l *Listeners) shutdownLocked(bs *boundServer) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := bs.srv.Shutdown(ctx); err != nil {
		l.opts.Log.Printf("bridge: shutdown: %v", err)
	}
}
