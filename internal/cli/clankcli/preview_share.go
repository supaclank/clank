package clankcli

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/supaclank/clank/internal/sharetunnel"
)

// shareStartTimeout bounds the wait for cloudflared to print the
// public URL (edge handshake, normally a couple of seconds).
const shareStartTimeout = 30 * time.Second

// startShareTunnel publishes upstreamURL on a public quick-tunnel URL.
// It fronts the raw dev server, never the overlay proxy, so the link
// is view-only: no overlay, no agent, no daemon token.
func startShareTunnel(ctx context.Context, upstreamURL *url.URL) (*sharetunnel.Tunnel, error) {
	bin, err := sharetunnel.FindBinary()
	if err != nil {
		return nil, err
	}
	fmt.Println("Publishing the app on a public share URL…")
	startCtx, cancel := context.WithTimeout(ctx, shareStartTimeout)
	defer cancel()
	tun, err := sharetunnel.Start(startCtx, bin, upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("start share tunnel: %w", err)
	}
	return tun, nil
}

// warnOnShareTunnelExit tells the user their public link died
// mid-session (edge quota, network, crash). Quiet during normal
// shutdown, when the Stop defer is what ends the process.
func warnOnShareTunnelExit(sigCtx context.Context, tun *sharetunnel.Tunnel) {
	select {
	case <-sigCtx.Done():
	case <-tun.Done():
		if sigCtx.Err() != nil {
			return
		}
		msg := "the public share URL is gone (cloudflared exited"
		if err := tun.Err(); err != nil {
			msg += ": " + err.Error()
		}
		msg += ") — restart clank preview --share for a new link"
		fmt.Println(styleWarn.Render(msg))
	}
}
