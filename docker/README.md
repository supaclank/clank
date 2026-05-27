# Self-hosted clank stack (docker compose)

Brings up a complete clank backend on your laptop (primarily for development & testing purposes, and as an example setup):

- **minio** — S3-compatible object storage for checkpoint bundles
- **clankd** — gateway with the embedded sync server (presigned URLs +
  sqlite metadata) and the local provisioner (spawns clank-host as a
  subprocess inside the container so migrations land somewhere)
- **clank-auth-stub** — dev OAuth 2.0 + PKCE server that
  auto-approves every authorization and mints an HS256-signed JWT, so
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

# Register the docker stack as a remote (one-time). `clank login`
# discovers the auth-stub via the gateway's /auth-config endpoint —
# no separate auth URL flag needed.
clank remote add dev --gateway-url=http://localhost:7878

# Sign in via OAuth 2.0 + PKCE against the stub. The browser auto-
# approves (no user interaction in dev) and the laptop receives an
# HS256 JWT that clankd verifies with its shared secret:
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
# and spawns the configured agent backend there (both `opencode` and
# `claude` are baked into the image) — no clone, no GitHub auth needed.
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

### Editing preferences

`docker/preferences.json` is created by `make docker-up` from
`docker/preferences.example.json` on first run and gitignored after
that — edit your local copy freely without polluting diffs. To pick
up changes, restart the stack (`make docker-down && make docker-up`).

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

## Testing the preview-app feature locally

The tokenized preview surface (`preview-<token>.<root>/...`) is
disabled by default. To enable it locally:

```sh
# docker/.env (or your shell)
export CLANK_PREVIEW_ROOT_DOMAIN=localhost
export CLANK_PREVIEW_SIGNING_KEY=$(openssl rand -hex 32)   # optional; gateway generates one if empty
# CLANK_PREVIEW_WEBHOOK_URL has a sensible default in docker-compose.yml
```

Then `make dev-rebuild`. Verify the gateway log emits:

```
gateway: preview surface enabled on *.localhost
```

### How to use it

1. From a browser pointed at `http://localhost:7878/`, log in via the
   auth-stub and pick a worktree with an Expo project.
2. Trigger `POST /v1/worktrees/<wid>/preview/start` (via the mobile
   client or curl with your bearer). The response includes
   `{token, url, expires_at}` — `url` is `https://preview-<token>.localhost/`.
3. **Strip the `https://` and add the port:** open
   `http://preview-<token>.localhost:7878/` in your browser. `*.localhost`
   resolves to 127.0.0.1 in every modern browser without an
   `/etc/hosts` edit.
4. For signed-URL access without a JWT header (Expo dev-launcher path),
   call `POST /v1/preview/tokens/<token>/sign` to mint a `?clank_sig=…&
   clank_exp=…` URL; that's the URL the dev-launcher receives.

### Known local-dev caveats

- **Cookies need TLS in production but not on localhost.** The
  gateway detects HTTP and drops `Secure` on the auth cookies, so
  the signed-URL → cookie flow works on plain `http://*.localhost`.
- **Mobile dev on a real device** needs the preview hostname to
  resolve to your laptop. `*.localhost` only resolves to 127.0.0.1
  on the device running the browser — phones see "Unable to resolve
  host" because no public DNS knows about it. Three working options:
  - **nip.io / sslip.io** (recommended for Tailscale or LAN dev):
    `CLANK_PREVIEW_ROOT_DOMAIN=100-123-16-31.nip.io` resolves
    `*.100-123-16-31.nip.io` to `100.123.16.31`. Substitute YOUR
    laptop's reachable IP. The phone hits the laptop on port 7878
    directly, no tunnel daemon needed.
  - **Android emulator**: use `10.0.2.2` to reach the host;
    `CLANK_PREVIEW_ROOT_DOMAIN=10.0.2.2.nip.io`.
  - **Cloudflared / ngrok tunnel** (works from any network,
    including cellular): `cloudflared tunnel --url http://localhost:7878`,
    then set `CLANK_PREVIEW_ROOT_DOMAIN` to the tunnel hostname AND
    `CLANK_PREVIEW_WEBHOOK_URL` to the tunnel URL.
- **Sprite-side WSS tunnel** is bypassed: the local provisioner's
  `OpenInternalConn` direct-dials 127.0.0.1 inside the clankd
  container (clank-host runs as a subprocess there). The full
  WSS-tunnel-to-sprite path is only exercised by the cloud
  deployment.
- **Signing key persistence.** Without `CLANK_PREVIEW_SIGNING_KEY`
  set, a container restart invalidates every outstanding signed
  URL. Pin it for repeatable dev sessions.

## Tearing down

```sh
docker compose -f docker/docker-compose.yml down            # stop + remove containers
docker compose -f docker/docker-compose.yml down -v         # also drop volumes (resets all state)
```
