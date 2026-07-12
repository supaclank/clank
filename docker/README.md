# Self-hosted clank stack (docker compose)

Brings up a complete clank backend on your laptop (primarily for development & testing purposes, and as an example setup):

- **minio** — S3-compatible object storage for image uploads
- **clankd** — gateway + the local provisioner (spawns clank-host as a
  subprocess inside the container)
- **clank-auth-stub** — dev OAuth 2.0 + PKCE server that
  auto-approves every authorization and mints an HS256-signed JWT, so
  `clank login` works end-to-end against the local stack

Everything is self-contained — no fly.io, daytona, or AWS account
needed. Useful for smoke-testing the gateway + host flows end-to-end.

## One-time setup

There are two dev modes depending on where your sandbox runs.

### Mode A — laptop-only (`local` provisioner, sandbox in the clankd container)

```sh
make docker-setup    # adds `127.0.0.1 clank-minio` to /etc/hosts (sudo prompt)
make docker-up
```

The /etc/hosts entry makes `clank-minio` resolve to localhost from the
laptop, matching how the docker network resolves it from inside.

### Mode B — real cloud sandbox (`flysprites` / `daytona` provisioner)

A remote sprite can't resolve `clank-minio` — it lives on its own
network with no host-file injection. It needs a **publicly-reachable,
real bucket**: provision an S3-compatible bucket (S3, R2, …), set
`CLANK_IMAGES_S3_ENDPOINT` + `CLANK_IMAGES_S3_PUBLIC_ENDPOINT` in
`docker/.env` to its endpoint, then `make dev-rebuild`.

### Why presigned URLs need one hostname

clankd's image presigner mints SigV4-signed presigned URLs. SigV4 signs
the **Host** header into the canonical request, so the URL bears one
hostname and every consumer (laptop, sprite) must dial that exact
name. Rewriting the host on a consumer would invalidate the
signature.

The gateway itself dials minio at the docker-internal hostname
(`http://clank-minio:9000`, `CLANK_IMAGES_S3_ENDPOINT`) for its own
direct SDK calls and mints presigned URLs with
`CLANK_IMAGES_S3_PUBLIC_ENDPOINT`. For local dev leave that empty — it
falls back to the same internal hostname, which both the laptop
(`127.0.0.1 clank-minio` in /etc/hosts) and the in-container sprite
resolve, so one hostname satisfies every consumer with no tunnel.

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

## Smoke-testing against the stack

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

# Import a repo / scaffold a project through the repo-first surface
# (this is what the mobile app calls):
curl -X POST http://localhost:7878/v1/projects/import \
  -H "Authorization: Bearer <access_token>" \
  -d '{"owner":"<you>","repo":"<repo>"}'

# The host lays the repo out as a bare canonical + linked worktree:
docker compose -f docker/docker-compose.yml exec clankd ls /root/work/
docker compose -f docker/docker-compose.yml exec clankd ls /root/work/repos/
```

## What's actually self-hosted

All HTTP services run in containers. Outbound traffic happens only
for GitHub clones/pushes on the host and presigned minio URLs for
image uploads. No secrets ever leave the docker network.

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
  "default_launch_host_provider": "flysprites",
  "flysprites": {
    "api_token": "<fly-api-token>",
    "organization_slug": "<slug>"
  }
}
```

If the sprite needs to call back into clankd (notifications,
preview webhooks), set `CLANK_PUBLIC_BASE_URL` in `docker/.env` to a
publicly-reachable URL of clankd — easiest is a cloudflared tunnel:

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
