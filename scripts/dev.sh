#!/bin/bash
# dev.sh — one-shot local dev environment for clank.
#
# Spawns a Cloudflare quick tunnel for minio (port 9000). The minio
# tunnel exists because presigned S3 URLs bind the bucket hostname
# into the SigV4 signature, so the laptop and a remote sandbox (fly.io)
# must dial the same hostname for the signature to verify. The sprite
# never reaches back to clankd directly — gateway-orchestrated pull
# means the only outbound the sprite makes is to S3 via short-lived
# presigned URLs.
#
# Captures the URL, writes it to docker/.env, then brings the stack up.
# Foreground; ctrl-c tears the tunnel + stack down together. Quick
# tunnels rotate per restart, so re-run if you stop and start.

set -eu

MINIO_PORT="${MINIO_API_PORT:-9000}"
CLANKD_PORT="${CLANKD_PORT:-7878}"
COMPOSE="docker compose -f docker/docker-compose.yml"
ENV_FILE="docker/.env"

# start_tunnel <port> <url_var_name> <pid_var_name>
# Spawns cloudflared on the given port and waits up to 60s for it to
# print a trycloudflare URL. Stores the URL + cloudflared PID into the
# named variables in the *parent* scope (printf -v) so the backgrounded
# process stays a child of the running shell — `wait` depends on that.
start_tunnel() {
        local port="$1"
        local url_var="$2"
        local pid_var="$3"
        local log
        log="$(mktemp)"
        cloudflared tunnel --url "http://localhost:$port" --no-autoupdate \
                > "$log" 2>&1 &
        local pid=$!
        local i=0
        local url=""
        while [ $i -lt 120 ]; do
                if ! kill -0 "$pid" 2>/dev/null; then
                        echo "ERROR: cloudflared for :$port exited early. Log:" >&2
                        cat "$log" >&2
                        rm -f "$log"
                        exit 1
                fi
                url="$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$log" | head -1 || true)"
                if [ -n "$url" ]; then
                        rm -f "$log"
                        printf -v "$url_var" '%s' "$url"
                        printf -v "$pid_var" '%s' "$pid"
                        return
                fi
                sleep 0.5
                i=$((i + 1))
        done
        echo "ERROR: cloudflared for :$port did not advertise a URL within 60s. Log:" >&2
        cat "$log" >&2
        rm -f "$log"
        exit 1
}

echo "Spawning cloudflared tunnel for minio (:$MINIO_PORT)..."
start_tunnel "$MINIO_PORT" MINIO_URL MINIO_PID

cleanup() {
        trap '' INT TERM EXIT
        echo
        echo "Stopping cloudflared and docker stack..."
        kill "$MINIO_PID" 2>/dev/null || true
        $COMPOSE down
}
trap cleanup INT TERM EXIT

echo "Tunnel ready: $MINIO_URL (pid=$MINIO_PID)"

# Seed docker/.env from .env.example on first run, then upsert the
# tunnel URL. Rewrite via tmp to avoid os-specific sed -i differences.
mkdir -p docker
if [ ! -f "$ENV_FILE" ] && [ -f docker/.env.example ]; then
        cp docker/.env.example "$ENV_FILE"
fi
touch "$ENV_FILE"
TMP_ENV="$(mktemp)"
grep -v '^CLANK_SYNC_S3_PUBLIC_ENDPOINT=' "$ENV_FILE" \
        | grep -v '^CLANK_SYNC_S3_ENDPOINT=' \
        | grep -v '^CLANK_PUBLIC_BASE_URL=' \
        > "$TMP_ENV" || true
echo "CLANK_SYNC_S3_PUBLIC_ENDPOINT=$MINIO_URL" >> "$TMP_ENV"
mv "$TMP_ENV" "$ENV_FILE"
echo "Updated $ENV_FILE."

echo "Starting docker stack..."
$COMPOSE --env-file "$ENV_FILE" up -d --build

echo
echo "=========================================="
echo " Dev stack ready."
echo
echo "   minio tunnel  $MINIO_URL    (presigned URLs sign for this host)"
echo "   Gateway       http://localhost:${CLANKD_PORT}"
echo "   Auth stub     http://localhost:${CLANK_AUTH_STUB_PORT:-7879}"
echo "   MinIO         http://localhost:$MINIO_PORT  (console :${MINIO_CONSOLE_PORT:-9001})"
echo
echo " On the laptop, register the remote once and sign in:"
echo
echo "   clank remote add dev \\"
echo "     --gateway-url=http://localhost:${CLANKD_PORT} \\"
echo "     --auth-url=http://localhost:${CLANK_AUTH_STUB_PORT:-7879}"
echo "   clank login   # auth-stub mints an HS256 JWT the gateway verifies"
echo
echo " ctrl-c to tear down tunnel + stack."
echo "=========================================="

# Block on the tunnel. cloudflared is a child of this shell (start_tunnel
# spawned it in our scope), so wait blocks until it exits or we trap a
# signal.
wait "$MINIO_PID"
