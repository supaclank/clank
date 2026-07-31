# clank.yaml

`clank.yaml` is an optional, user-written config file at the root of a
repo (the folder you run `clank preview` from). Without it, previews
auto-detect everything; with it, any stack can preview. clank never
writes this file.

The file is organized as independent top-level sections so future
features can join as siblings — and parsing is strict at every level:
a typo'd key or section fails loudly, and a clank binary older than a
repo's config refuses unknown sections ("upgrade clank") instead of
silently ignoring semantics it doesn't understand.

## `preview`

```yaml
preview:
  # Repo-relative subdirectory to preview from. Lets you run
  # `clank preview` at the monorepo root while the app lives in
  # web-app/. Detection, installs, and the dev server all run there.
  dir: web-app

  # Dependency-install command, run through `sh -c` in the preview
  # dir before the dev server starts. Replaces the install clank
  # would otherwise synthesize from your lockfile — and ONLY the
  # install: it's arbitrary shell, so clank never parses it to infer
  # how tools launch or which binaries it needs. Gated by a
  # completion marker outside your repo, and never wipes a
  # node_modules it didn't build. If your package.json declares a
  # manager clank can't launch tools for (say deno), set `command`
  # alongside it.
  install: pnpm install

  # Dev-server command, run through `sh -c` in the preview dir. Must
  # contain ${PORT} — clank allocates the port and substitutes every
  # occurrence. Setting this bypasses framework detection entirely
  # and always uses the browser preview flow. Without `install`, the
  # command owns its own dependency setup.
  #
  # Bind 127.0.0.1 on that port: the readiness probe and the overlay
  # proxy dial IPv4 loopback, and Node ≥ 17 may resolve a bare
  # "localhost" as ::1 only.
  command: ./scripts/dev.sh --listen 127.0.0.1:${PORT}

  # Readiness probe override: GET path on the allocated port until it
  # returns 200 and (when expect is set) the body contains expect.
  # Default for custom commands: 200 on /.
  ready:
    path: /healthz
    expect: ok
```

Every key is optional and they compose: `dir` alone re-roots
auto-detection into a subdirectory; `install` alone swaps the install
step under a detected framework; `command` takes over the dev server
entirely.

## Zero-config behavior (no clank.yaml)

`clank preview` detects Expo (phone flow), Next.js, and Vite (browser
flow) projects. Install and launch are resolved independently: the
launch always follows the repo's layout (`yarn <tool>` for yarn/PnP
repos, `node_modules/.bin/<tool>` for everything else), while the
install depends on where the preview runs.

**On your laptop** (previewing your own checkout), clank treats the
tree as yours:

- Dependencies already installed — a `node_modules` tree, or yarn PnP
  artifacts — are used as-is; clank never installs over them or
  reconciles them.
- A missing tree is created with the project's own manager, resolved
  by `package.json` `packageManager` (the corepack convention, e.g.
  `"pnpm@9.1.0"`) / `devEngines.packageManager`, then the lockfile
  (`bun.lock`/`bun.lockb` → bun, `pnpm-lock.yaml` → pnpm, `yarn.lock`
  → yarn, `package-lock.json` → npm; nearest directory wins, walking
  up to the repo root), else bun (clank-created templates ship no
  lockfile; bun installs are roughly 10× faster than npm on cold
  worktrees).
- When a lockfile exists, the install runs in the manager's frozen
  mode (`npm ci`, `pnpm install --frozen-lockfile`, `yarn install
  --frozen-lockfile`, `bun install --frozen-lockfile`): a drifted
  lockfile fails loudly instead of clank dirtying your checkout.

**In the cloud**, installs use bun regardless of the repo — machine
worktrees are materialized fresh from git, and bun's speed and
hard-linked disk usage are what an I/O-constrained machine needs.

`preview.install` overrides the install everywhere — laptop and cloud
alike (e.g. a pnpm-workspace repo that can't install under bun). A
detected manager that isn't installed fails the preview start with
the evidence in the error — `preview.install` sidesteps it.

## Element-to-source resolution

The browser overlay's click-to-source works best on Svelte, React
(≤18 and 19), Vite-served, and Next.js apps, where dev-mode metadata
pins the exact file and line. Anything else still previews: the
overlay falls back to a DOM path plus an HTML snippet, which is
enough context for the agent to find the code.
