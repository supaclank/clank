#!/bin/sh
# Entrypoint for the clank-host sandbox image. Reads cloud-hub
# coordinates from env, then invokes clank-host with the matching
# flags.
#
# Required env:
#   CLANK_HUB_URL    — cloud hub's externally-reachable base URL.
#                      In dev: typically the trycloudflare URL emitted
#                      by `make cloud-hub`. In prod: the deployed cloud
#                      hub's public domain.
#   CLANK_HUB_TOKEN  — bearer token paired with CLANK_HUB_URL.
#   CLANK_HOST_PORT  — TCP port to bind. Defaults to 7878.
#
# Failures are loud: missing required env crashes the container so
# the sandbox host (Daytona, etc.) surfaces the problem instead of
# leaving an idle sandbox.

set -eu

: "${CLANK_HUB_URL:?CLANK_HUB_URL is required}"
: "${CLANK_HUB_TOKEN:?CLANK_HUB_TOKEN is required}"
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
  --git-sync-source "${CLANK_HUB_URL}" \
  --git-sync-token "${CLANK_HUB_TOKEN}"
