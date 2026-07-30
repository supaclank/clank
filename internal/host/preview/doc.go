// Package preview spawns and supervises per-worktree development
// servers (Expo's Metro, Next.js, Vite, or a clank.yaml-declared
// command) inside the sprite. The gateway reaches these servers via
// Sprites' WSS proxy and routes traffic to them based on a tokenized
// public URL — clank-host itself no longer proxies preview traffic.
//
// The goal is the "▶ Preview app" affordance on the mobile client:
// tapping it brings up a running dev server in the user's sprite
// without a laptop in the loop. Detect recognizes Expo (phone flow)
// plus Next.js and Vite (browser flow), installing through the
// project's own package manager (ResolvePackager); every other stack
// declares its dev server via clank.yaml's preview section
// (internal/clankyaml, docs/clank-yaml.md), which also re-roots
// monorepo subdirectory previews and overrides installs and readiness
// probes. The registry stays keyed by (worktree, service) so
// multi-service previews can land without another refactor.
//
// Lifetime: Manager is constructed once per host.Service. It holds an
// in-memory map of running servers keyed by (worktree ID, service
// name); sprite hibernation kills the children and Manager respawns
// on the next /start request. The gateway tracks tokenized public
// URLs in its own persistent store (preview_routes); the sprite-side
// state here is process-bound only.
//
// Architecture:
//
//   - GWClient calls the gateway's /webhooks/preview/{register,revoke}
//     to mint and tear down public tokens.
//   - Start spawns the dev server, blocks on readiness, then calls
//     GWClient.Register to mint a token + public URL. Empty
//     GWClient = laptop dev: Status.Token/URL stay empty.
//   - Stop/Shutdown/reap call GWClient.Revoke so the gateway's row
//     is marked revoked the moment the sprite stops serving.
//
// # Future direction
//
// Long term, replace the sprite-side spawn supervisor with a
// gateway-owned manifest: the gateway becomes the reconciler that
// uses Sprites' CreateService / DeleteService API to spawn Metro as
// a first-class sprite service (own internal port, own restart
// policy, own dependency declaration). This package then shrinks to
// the Spec/Detect surface and goes away.
//
// That migration is forecast-friendly from where this code lives
// today: the (wid, service) key is already in place, the
// OpenInternalConn provisioner method already works for whatever
// port Sprites allocates, and the token + visibility model is
// gateway-owned. The mobile contract (`{preview_url, token,
// expires_at}` from /preview/start) doesn't change.
//
// Don't entrench process-supervisor responsibilities in this
// package — anything bigger than "spawn a Cmd and wait for /status"
// belongs at the gateway when that migration lands.
package preview
