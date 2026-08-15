package sharetunnel

import "regexp"

// quickTunnelURL matches the public origin cloudflared prints in its
// startup banner, e.g.
//
//	INF |  https://lively-fox-rain.trycloudflare.com  |
var quickTunnelURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// publicTunnelURL extracts the quick-tunnel origin from one log line.
func publicTunnelURL(line string) (string, bool) {
	u := quickTunnelURL.FindString(line)
	return u, u != ""
}
