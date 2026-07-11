package hostmux

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

// tunnelLocalDialTimeout caps the loopback dial. Well under the
// tunnel client's overall dial budget (previewtunnel's 5s) so a dead
// dev-server port answers as a fast 502 instead of eating the whole
// budget.
const tunnelLocalDialTimeout = 2 * time.Second

// handleTunnel bridges a WebSocket to a TCP connection on the host's
// own loopback: GET /tunnel/{port}. Binary frames are relayed
// byte-for-byte to 127.0.0.1:{port}, both directions, until either
// side closes.
//
// This is how the gateway reaches per-worktree dev servers (Metro
// etc.) on providers whose edge only routes declared service ports:
// the provisioner's OpenInternalConn dials this endpoint via
// pkg/provisioner/tunnelclient. Loopback-only by construction — the
// bearer holder can reach any local port, never another machine.
//
// The loopback dial happens BEFORE the WebSocket upgrade so "dev
// server not listening" surfaces as a plain HTTP 502 the tunnel
// client can tell apart from transport failures.
func (m *Mux) handleTunnel(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeJSON(w, http.StatusBadRequest, errResp{
			Code:  "invalid_argument",
			Error: fmt.Sprintf("tunnel: invalid port %q", r.PathValue("port")),
		})
		return
	}

	d := net.Dialer{Timeout: tunnelLocalDialTimeout}
	target, err := d.DialContext(r.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errResp{
			Code:  "port_unreachable",
			Error: fmt.Sprintf("tunnel: dial 127.0.0.1:%d: %v", port, err),
		})
		return
	}
	defer target.Close()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Server-to-server endpoint: the gateway dials with a bearer
		// token, there is no browser origin to verify.
		InsecureSkipVerify: true,
	})
	if err != nil {
		// Accept has already written an error response.
		m.log.Printf("tunnel: websocket accept for port %d: %v", port, err)
		return
	}
	// Proxied streams are opaque and unbounded (bundler payloads
	// exceed the 32KB default message limit).
	c.SetReadLimit(-1)

	ws := websocket.NetConn(r.Context(), c, websocket.MessageBinary)

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(target, ws); errc <- err }()
	go func() { _, err := io.Copy(ws, target); errc <- err }()
	// First EOF/error wins; closing both conns unblocks the other copy.
	<-errc
	_ = ws.Close()
	_ = target.Close()
	<-errc
}
