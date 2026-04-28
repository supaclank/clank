#!/usr/bin/env bash
# Local-dev helper: spin up a Cloudflare quick tunnel, then start
# `clankd` with the tunnel URL as --public-base-url. Both die
# together on Ctrl-C.
#
# This exists only for the "cloud hub running on my laptop"
# iteration loop. Production cloud hubs sit on a real public domain
# — sandboxes hit that domain directly, no tunnel needed. We use
# Cloudflare quick tunnels (no account required) because Tailscale
# is blocked by some sandbox runtimes (notably Daytona EU, where
# *.tailscale.com SNI is reset by network DPI), and quick tunnels
# pose as plain HTTPS to *.trycloudflare.com.
#
# Each invocation gets a fresh URL — quick tunnels are ephemeral
# by design. Restart this script ⇒ new URL ⇒ all clients
# (mobile, future TUI) reconnect against the new URL after a
# clankd restart anyway.
#
# Knobs (env vars):
#   CLANK_DIR  data dir for the cloud hub. Default: ~/.clank-cloud
#   LISTEN     listener address. Default: tcp://0.0.0.0:7878
#              (NB: 0.0.0.0, not 127.0.0.1 — cloudflared connects
#              via localhost either way, but binding broadly leaves
#              the door open for direct LAN tests.)

set -euo pipefail

CLANK_DIR="${CLANK_DIR:-$HOME/.clank-cloud}"
LISTEN="${LISTEN:-tcp://0.0.0.0:7878}"

# Extract the port from `tcp://host:port` so we know what cloudflared
# should forward to.
PORT="${LISTEN##*:}"
if ! [[ "$PORT" =~ ^[0-9]+$ ]]; then
    echo "could not parse port from LISTEN=$LISTEN" >&2
    exit 1
fi

command -v cloudflared >/dev/null 2>&1 || {
    cat >&2 <<'MSG'
cloudflared not found in PATH.
  macOS: brew install cloudflared
  linux: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/
MSG
    exit 1
}
command -v clankd >/dev/null 2>&1 || {
    echo "clankd not found in PATH. Run 'make install' first." >&2
    exit 1
}

TUNNEL_LOG="$(mktemp -t clank-tunnel.XXXXXXXX)"
TUNNEL_PID=""
cleanup() {
    if [ -n "$TUNNEL_PID" ]; then
        kill "$TUNNEL_PID" 2>/dev/null || true
    fi
    # Leave the log on disk if cloudflared crashed before clankd
    # started — that's where the diagnostic message will tell the
    # user to look. Delete it on a clean exit.
    if [ "${KEEP_TUNNEL_LOG:-0}" != "1" ]; then
        rm -f "$TUNNEL_LOG"
    fi
}
trap cleanup EXIT INT TERM

echo "Starting cloudflared quick tunnel against http://localhost:$PORT ..."
cloudflared tunnel --url "http://localhost:$PORT" --no-autoupdate \
    > "$TUNNEL_LOG" 2>&1 &
TUNNEL_PID=$!

# Quick tunnels typically advertise their URL within 2-5 seconds.
# 45s is a generous upper bound for a slow network.
URL=""
for _ in $(seq 1 90); do
    if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
        echo "ERROR: cloudflared exited before advertising a URL. Output:" >&2
        cat "$TUNNEL_LOG" >&2
        KEEP_TUNNEL_LOG=1
        exit 1
    fi
    URL="$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$TUNNEL_LOG" | head -1 || true)"
    [ -n "$URL" ] && break
    sleep 0.5
done
if [ -z "$URL" ]; then
    echo "ERROR: cloudflared did not advertise a URL within 45s. Output:" >&2
    cat "$TUNNEL_LOG" >&2
    KEEP_TUNNEL_LOG=1
    exit 1
fi

cat <<MSG

  Tunnel URL:  $URL
  CLANK_DIR:   $CLANK_DIR
  Listen:      $LISTEN
  Tunnel log:  $TUNNEL_LOG  (tail -f if cloudflared misbehaves)
  Press Ctrl-C to stop both clankd and cloudflared.

MSG

# Run clankd in the foreground. The shell forwards SIGINT to clankd;
# clankd's own signal handler triggers a graceful shutdown; when it
# returns, the EXIT trap above kills cloudflared.
env CLANK_DIR="$CLANK_DIR" \
    clankd start --foreground \
        --listen "$LISTEN" \
        --public-base-url "$URL"
