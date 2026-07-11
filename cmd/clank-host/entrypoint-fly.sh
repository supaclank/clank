#!/bin/sh
# Entrypoint for the Fly Machines clank-host image. The machine's
# volume is mounted at /data and IS the user's disk: HOME, worktrees
# (~/work), host.db and agent credentials all live on it; everything
# else is immutable image rootfs, so image bumps never touch state.
#
# Required env:
#   CLANK_HOST_AUTH_TOKEN — bearer the gateway uses to call this host.
#
# Optional:
#   CLANK_HOST_PORT        — TCP port to bind (default 8080).
#   CLANK_RESTORE_URL      — presigned GET of a .tar.gz to unpack into
#                            /data on FIRST boot only (sandbox
#                            migration). Ignored once initialized.
#   CLANK_KEEPALIVE_PROVIDER / CLANK_NOTIFIER_PROVIDER /
#   CLANK_NOTIFIER_WEBHOOK_URL / CLANK_NOTIFIER_TOKEN /
#   CLANK_PREVIEW_WEBHOOK_URL / CLANK_GITHUB_OAUTH_CLIENT_ID /
#   CLANK_HOST_DATA_DIR    — forwarded to clank-host.

set -eu

: "${CLANK_HOST_PORT:=8080}"
export HOME=/data

# First-boot volume seeding, marker-guarded so restarts and image
# updates never redo it (and never re-run a restore over live state).
if [ ! -f /data/.clank-initialized ]; then
  mkdir -p /data/work
  if [ -n "${CLANK_RESTORE_URL:-}" ]; then
    echo "entrypoint: restoring workspace archive into /data"
    curl -fsSL "$CLANK_RESTORE_URL" | tar -xz -C /data
    echo "entrypoint: restore complete"
  fi
  touch /data/.clank-initialized
fi

# Bind the IPv6 wildcard: dual-stack on Linux, and fly-proxy forwards
# to the machine's 6PN address. Provider knobs are forwarded only when
# set so the binary's own env defaults stay authoritative.
exec /usr/local/bin/clank-host \
  --listen "tcp://[::]:${CLANK_HOST_PORT}" \
  ${CLANK_KEEPALIVE_PROVIDER:+--keepalive-provider "${CLANK_KEEPALIVE_PROVIDER}"} \
  ${CLANK_NOTIFIER_PROVIDER:+--notifier-provider "${CLANK_NOTIFIER_PROVIDER}"} \
  ${CLANK_NOTIFIER_WEBHOOK_URL:+--notifier-webhook-url "${CLANK_NOTIFIER_WEBHOOK_URL}"}
