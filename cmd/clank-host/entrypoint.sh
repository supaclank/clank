#!/bin/sh
# Entrypoint for the clank-host sandbox image. Reads minimal env and
# invokes clank-host. The sprite-side sync flow (P6) doesn't need any
# long-lived sync credentials on the sandbox — the gateway hands them
# in per-request when it calls POST /sync/checkpoint.
#
# Required env:
#   CLANK_HOST_PORT  — TCP port to bind. Defaults to 7878.
#
# Optional:
#   CLANK_HOST_AUTH_TOKEN — bearer the gateway uses to call this host.
#                            Empty preserves the unauthenticated
#                            laptop-local mode.

set -eu

: "${CLANK_HOST_PORT:=7878}"

# Bind on the IPv6 wildcard. On Linux this is dual-stack by default
# (IPV6_V6ONLY=0), so the listener accepts both IPv4 and IPv6
# connections. Daytona's toolbox proxy dials `[::1]:<port>` (IPv6
# loopback) when forwarding preview-URL traffic into the sandbox; an
# IPv4-only `0.0.0.0` bind would be unreachable and the proxy would
# return 502s. Sticking to `[::]` here also keeps macOS happy (where
# `0.0.0.0` and `::` behave nearly identically).
exec /usr/local/bin/clank-host \
  --listen "tcp://[::]:${CLANK_HOST_PORT}" \
  ${CLANK_HOST_AUTH_TOKEN:+--listen-auth-token "${CLANK_HOST_AUTH_TOKEN}"}
