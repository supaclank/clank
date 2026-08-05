#!/bin/bash
# dev.sh — one-shot local dev environment for clank.
#
# Brings up the docker stack (gateway + minio + auth-stub) in the foreground;
# ctrl-c tears it down.
#
# Presigned S3 URLs resolve to the INTERNAL minio (clank-minio:9000): the
# in-container sprite reaches it via docker DNS, and the laptop via the
# `127.0.0.1 clank-minio` entry that `make docker-setup` adds to /etc/hosts.
# Both dial the same hostname, so the SigV4 signature verifies and no public
# tunnel is needed for local development. When CLANK_IMAGES_S3_PUBLIC_ENDPOINT is
# unset the gateway falls back to the internal endpoint automatically.
#
# Testing against a REAL remote sprite (fly.io etc.) needs a publicly-reachable
# bucket — provision a real S3/R2 bucket and set CLANK_IMAGES_S3_ENDPOINT +
# CLANK_IMAGES_S3_PUBLIC_ENDPOINT in docker/.env. Cloudflare quick-tunnels were
# removed: they are not built for blob transfer (low throughput, and the
# rotating URLs went stale and failed mid-sync with s3_unreachable).

set -eu

COMPOSE="docker compose -f docker/docker-compose.yml"
ENV_FILE="docker/.env"

# Seed docker/.env from the example on first run.
mkdir -p docker
if [ ! -f "$ENV_FILE" ] && [ -f docker/.env.example ]; then
	cp docker/.env.example "$ENV_FILE"
	echo "Seeded $ENV_FILE from docker/.env.example."
fi
touch "$ENV_FILE"

cleanup() {
	trap '' INT TERM EXIT
	echo
	echo "Stopping docker stack..."
	$COMPOSE down
}
trap cleanup INT TERM EXIT

echo "Starting docker stack..."
$COMPOSE --env-file "$ENV_FILE" up -d --build

CLANKD_PORT="${CLANKD_PORT:-7878}"
echo
echo "=========================================="
echo " Dev stack ready."
echo
echo "   Gateway       http://localhost:${CLANKD_PORT}"
echo "   Auth stub     http://localhost:${CLANK_AUTH_STUB_PORT:-7879}"
echo "   MinIO         http://localhost:${MINIO_API_PORT:-9000}  (console :${MINIO_CONSOLE_PORT:-9001})"
echo
echo " Presigned S3 URLs resolve to clank-minio:9000 — needs '127.0.0.1 clank-minio'"
echo " in /etc/hosts (run 'make docker-setup' once if push/pull 401s or can't dial S3)."
echo
echo " On the laptop, register the remote once and sign in:"
echo
echo "   clank remote add dev \\"
echo "     --gateway-url=http://localhost:${CLANKD_PORT} \\"
echo "     --auth-url=http://localhost:${CLANK_AUTH_STUB_PORT:-7879}"
echo "   clank login   # auth-stub mints an HS256 JWT the gateway verifies"
echo
echo " ctrl-c to tear down the stack. Following gateway logs:"
echo "=========================================="
echo

# Block in the foreground tailing the gateway; ctrl-c triggers cleanup (down).
$COMPOSE --env-file "$ENV_FILE" logs -f clankd
