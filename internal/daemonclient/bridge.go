package daemonclient

import "context"

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

// BridgeStatus is the admin status payload. PairToken carries the
// root secret (unix-socket-only surface); URLs are the
// phone-reachable base URLs, best-first, empty when nothing beyond
// loopback is bound — the CLI's cue to run the trust-LAN prompt.
type BridgeStatus struct {
	Port           int            `json:"port"`
	Binds          []BridgeBind   `json:"binds"`
	Tailnet        *BridgeTailnet `json:"tailnet,omitempty"`
	LANIP          string         `json:"lan_ip,omitempty"`
	Network        BridgeNetwork  `json:"network"`
	NetworkTrusted bool           `json:"network_trusted"`
	FirstConnected bool           `json:"first_connected"`
	PairToken      string         `json:"pair_token"`
	URLs           []string       `json:"urls"`
}

// Status fetches (and freshly re-discovers) the bridge state.
func (b *BridgeClient) Status(ctx context.Context) (*BridgeStatus, error) {
	var s BridgeStatus
	if err := b.c.get(ctx, "/v1/bridge/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Rotate mints a new root secret, disconnecting every phone.
func (b *BridgeClient) Rotate(ctx context.Context) (*BridgeStatus, error) {
	var s BridgeStatus
	if err := b.c.post(ctx, "/v1/bridge/rotate", nil, &s); err != nil {
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
