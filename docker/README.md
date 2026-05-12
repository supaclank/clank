# Self-hosted clank stack (docker compose)

Brings up a complete clank backend on your laptop (primarily for development & testing purposes, and as an example setup):

- **minio** — S3-compatible object storage for checkpoint bundles
- **clankd** — gateway with the embedded sync server (presigned URLs +
  sqlite metadata) and the local provisioner (spawns clank-host as a
  subprocess inside the container so migrations land somewhere)
- **clank-auth-stub** — dev OAuth 2.0 device-flow server that
  auto-approves every login and mints an HS256-signed JWT, so
  `clank login` works end-to-end against the local stack

Everything is self-contained — no fly.io, daytona, or AWS account
needed. Useful for smoke-testing the sync/migration flow end-to-end.

## One-time setup

There are two dev modes depending on where your sandbox runs.

### Mode A — laptop-only (`local` provisioner, sandbox in the clankd container)

```sh
make docker-setup    # adds `127.0.0.1 clank-minio` to /etc/hosts (sudo prompt)
make docker-up
```

The /etc/hosts entry makes `clank-minio` resolve to localhost from the
laptop, matching how the docker network resolves it from inside.

### Mode B — real cloud sandbox (`flyio` / `daytona` provisioner)

A fly.io sprite can't resolve `clank-minio` — it lives on its own
network with no host-file injection. Expose minio publicly via a
Cloudflare quick tunnel and point clankd at the public URL.

```sh
make dev
```

That spawns cloudflared, captures the trycloudflare URL, writes it to
`docker/.env` as `CLANK_SYNC_S3_ENDPOINT`, and brings the stack up.
Foreground; ctrl-c tears down the tunnel + the docker stack together.
Quick tunnels rotate per restart so re-run `make dev` if you stop and
start again.

If you want to manage the tunnel yourself (e.g. a stable Cloudflare
named tunnel), `make tunnel` runs just cloudflared and prints the URL
for you to paste into `docker/.env` manually.

### Why presigned URLs need one hostname

clankd's embedded sync mints SigV4-signed presigned URLs. SigV4 signs
the **Host** header into the canonical request, so the URL bears one
hostname and every consumer (laptop, sprite) must dial that exact
name. Rewriting the host on a consumer would invalidate the
signature.

The gateway itself short-circuits this: it dials minio at the
docker-internal hostname (`http://clank-minio:9000`) for its own
direct SDK calls (HeadObject at commit time, etc.) while minting
presigned URLs with `CLANK_SYNC_S3_PUBLIC_ENDPOINT` — usually the
cloudflared tunnel URL. Avoids round-tripping its own traffic
through cloudflared, which would rewrite the Host header and trip
the SigV4 check.

## Bringing the stack up

```sh
make docker-up      # implies docker-setup; builds + starts everything
make docker-logs    # tail logs from all services
make docker-down    # stop + remove containers
```

Health-checks:

```sh
# /ping and /v1/health are behind the gateway's auth middleware in
# TCP mode, so a bearer is required. After `clank login` (below),
# the simplest check is:
clank                                          # exits 0 if the active remote is reachable + authed
open http://localhost:9001                     # minio console (clankadmin / clankadmin)
```

Logs:

```sh
docker compose -f docker/docker-compose.yml logs -f clankd
```

## Smoke-testing the migration flow

The dev stack runs the gateway in HS256 JWT mode
(`CLANK_AUTH_JWT_SECRET` in `docker-compose.yml`, wired to the
auth-stub's `CLANK_AUTH_STUB_SECRET`). `clank login` against the
auth-stub mints an HS256-signed JWT that the gateway verifies with
the same secret — no shared static token to keep in sync; the
laptop's `access_token` is the issued JWT.

Other modes (OIDC, opt-in static bearer) are mutually exclusive —
see [internal/cli/daemoncli/auth.go](../internal/cli/daemoncli/auth.go)
for the env-var selection algorithm.

From the laptop, with the stack running:

```sh
cd ~/some-real-repo

# Register the docker stack as a remote (one-time).
# --auth-url points at the dev auth-stub so `clank login` works.
clank remote add dev \
  --gateway-url=http://localhost:7878 \
  --auth-url=http://localhost:7879

# Sign in via device flow against the stub (auto-approves and mints
# an HS256 JWT the gateway will verify):
clank login

# Verify:
clank remote -v
# * dev	http://localhost:7878  dev@clank.local

# 1. Push a checkpoint AND hand off ownership to the remote.
clank push --migrate

# Output:
#   registered worktree 01J… as 'some-real-repo'
#   pushed checkpoint   01J… (HEAD a1b2c3d4)
#   migrated worktree   01J… → remote/<host_id>
#
# The bundles + manifest live in minio; the remote host has the
# materialized worktree at /root/work/<id>:
docker compose -f docker/docker-compose.yml exec -T minio \
  mc ls --recursive local/clank/checkpoints/
docker compose -f docker/docker-compose.yml exec clankd ls /root/work/

# 2. Open a session against the synced worktree. clank-host inside
# the clankd container resolves the WorktreeID to /root/work/<id>/
# and spawns opencode there — no clone, no GitHub auth needed.
clank code "summarize this codebase"

# 3. When you want to keep working on the laptop, reclaim ownership.
clank pull --migrate
```

## What's actually self-hosted

All HTTP services run in containers. Outbound traffic happens only
when the laptop pushes/pulls bundles via presigned minio URLs. No
secrets ever leave the docker network.

The "sandbox" in the default setup is a clank-host subprocess inside
the clankd container (the `local` provisioner) — useful for
end-to-end smoke testing but not what you'd run in production.

### Switching to fly.io provisioner

Edit `docker/preferences.json`:

```json
{
  "default_launch_host_provider": "flyio",
  "flyio": {
    "api_token": "<fly-api-token>",
    "organization_slug": "<slug>"
  }
}
```

When you start exercising sprite-side push (P6), set
`CLANK_PUBLIC_BASE_URL` in `docker/.env` to a publicly-reachable URL
of clankd — easiest is a cloudflared tunnel:

```sh
cloudflared tunnel --url http://localhost:7878
# then: CLANK_PUBLIC_BASE_URL=https://your-tunnel.trycloudflare.com
```

## Tearing down

```sh
docker compose -f docker/docker-compose.yml down            # stop + remove containers
docker compose -f docker/docker-compose.yml down -v         # also drop volumes (resets all state)
```
