package daemonclient

import (
	"context"
	"time"
)

// BridgeClient is the local-CLI handle for the daemon's laptop↔phone
// bridge (`clank pair`, `clank preview`'s QR building). Admin routes
// are unix-socket only — the daemon never mounts them on TCP.
type BridgeClient struct {
	c *Client
}

// Bridge returns the bridge admin handle.
func (c *Client) Bridge() *BridgeClient {
	return &BridgeClient{c: c}
}

// BridgeBind is one address the bridge listener policy wanted, and
// how the bind went ("" = serving).
type BridgeBind struct {
	IP     string `json:"ip"`
	Reason string `json:"reason"`
	Err    string `json:"err,omitempty"`
}

// BridgeTailnet mirrors the daemon's tailnet discovery.
type BridgeTailnet struct {
	IP      string `json:"ip"`
	DNSName string `json:"dns_name,omitempty"`
}

// BridgeNetwork identifies the current LAN for per-network trust.
type BridgeNetwork struct {
	Fingerprint string `json:"fingerprint,omitempty"`
	Label       string `json:"label,omitempty"`
}

// BridgeDevice is one approved phone in the daemon's registry.
type BridgeDevice struct {
	PubKey   string     `json:"pubkey"`
	Name     string     `json:"name"`
	AddedAt  time.Time  `json:"added_at"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// BridgeStatus is the admin status payload — all public: HostKey is
// the laptop's identity public key (the QR's hk param), Devices the
// approved registry, URLs the phone-reachable base URLs, best-first,
// empty when nothing beyond loopback is bound — the CLI's cue to run
// the trust-LAN prompt.
type BridgeStatus struct {
	Port           int            `json:"port"`
	Binds          []BridgeBind   `json:"binds"`
	Tailnet        *BridgeTailnet `json:"tailnet,omitempty"`
	LANIP          string         `json:"lan_ip,omitempty"`
	Network        BridgeNetwork  `json:"network"`
	NetworkTrusted bool           `json:"network_trusted"`
	HostKey        string         `json:"host_key"`
	Devices        []BridgeDevice `json:"devices"`
	URLs           []string       `json:"urls"`
	// Most recent authenticated connection this daemon run — the CLI
	// waits on this to clear the QR and name the phone.
	LastDevice      string     `json:"last_device,omitempty"`
	LastConnectedAt *time.Time `json:"last_connected_at,omitempty"`
}

// Status fetches (and freshly re-discovers) the bridge state.
func (b *BridgeClient) Status(ctx context.Context) (*BridgeStatus, error) {
	var s BridgeStatus
	if err := b.c.get(ctx, "/v1/bridge/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RevokeDevice removes one approved phone by its public key.
func (b *BridgeClient) RevokeDevice(ctx context.Context, pubkey string) (*BridgeStatus, error) {
	var s BridgeStatus
	if err := b.c.post(ctx, "/v1/bridge/pair/revoke", map[string]any{"pubkey": pubkey}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RevokeAllDevices removes every approved phone. The host key stays —
// returning phones still recognize the laptop, they just re-pair.
func (b *BridgeClient) RevokeAllDevices(ctx context.Context) (*BridgeStatus, error) {
	var s BridgeStatus
	if err := b.c.post(ctx, "/v1/bridge/pair/revoke", map[string]any{"all": true}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// TrustNetwork records LAN consent for the fingerprinted network and
// re-binds accordingly.
func (b *BridgeClient) TrustNetwork(ctx context.Context, fingerprint, label string) (*BridgeStatus, error) {
	var s BridgeStatus
	body := map[string]string{"fingerprint": fingerprint, "label": label}
	if err := b.c.post(ctx, "/v1/bridge/trust-network", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// PairPoll leases the pairing window open (the CLI calls it each tick
// while showing the QR) and returns the device names of phones waiting
// for the laptop user to type their code.
func (b *BridgeClient) PairPoll(ctx context.Context) ([]string, error) {
	var resp struct {
		Pending []string `json:"pending"`
	}
	if err := b.c.post(ctx, "/v1/bridge/pair/poll", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Pending, nil
}

// PairComplete submits the code the laptop user typed, approving the
// matching pending attempt. Returns the paired device name; a code
// that matches no waiting phone surfaces as an error (the transport
// maps the 400 body's message).
func (b *BridgeClient) PairComplete(ctx context.Context, code string) (string, error) {
	var resp struct {
		Device string `json:"device"`
	}
	if err := b.c.post(ctx, "/v1/bridge/pair/complete", map[string]string{"code": code}, &resp); err != nil {
		return "", err
	}
	return resp.Device, nil
}
