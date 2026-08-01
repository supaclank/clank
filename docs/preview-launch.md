# Preview launch configuration

Clank has three distinct preview modes:

- `clank preview . :5173` attaches the overlay proxy to a web server the user already runs. The folder supplies project and agent context. Clank does not start, health-check, or stop the upstream server.
- `clank preview` starts Expo automatically when the project is recognizable as Expo. This preserves the mobile flow.
- `clank preview` or `clank preview web-app` starts a configured web development server, waits for its declared HTTP readiness check, and owns its lifecycle.

Clank intentionally does not infer how arbitrary web projects install dependencies or start development servers. When a web launch configuration is missing, preview status returns `setup_required`, the two possible output paths, and an agent-ready setup prompt. The connected agent inspects the project once, writes the configuration, and verifies it. Runtime launches are deterministic after that.

## File format

The project file is `.clank/launch.yaml` at the repository root:

```yaml
default: web-app
previews:
  web-app:
    directory: web
    command: npm run dev -- --host 127.0.0.1 --port "$PORT"
    ready:
      path: /
  admin:
    directory: apps/admin
    command: pnpm dev --host 127.0.0.1 --port "$PORT"
    ready:
      path: /healthz
      expect: ok
```

Every field shown is required except `ready.expect`.

- `default` names the entry used by `clank preview`.
- `previews` maps CLI names to launch definitions. Names contain letters, numbers, `_`, or `-`. The name `default` is reserved for Expo's internal service; the top-level `default` field already makes any web entry the no-argument default.
- `directory` is relative to the project root. Absolute paths, parent traversal, and symlinks escaping the project are rejected.
- `command` is a one-line shell command run with `sh -c` from `directory`. It must consume the exact `$PORT` or `${PORT}` variable. An argv list would avoid shell parsing, but the one-line form is substantially easier to read and edit; setup verification catches project-specific quoting mistakes.
- `ready.path` is an HTTP path on `127.0.0.1:$PORT`. Readiness requires status 200. When `ready.expect` is present, the response body must also contain that text.

There is no preferred port or `autoPort`. Clank asks the operating system for an available port, exports it as `PORT`, and expects the configured command to bind that port. This removes framework-specific command rewriting and prevents silently connecting to an unrelated server already using a preferred port.

Setup should also configure the development server to bind `127.0.0.1`, not a LAN-facing address. Clank's readiness probe and proxy connect over IPv4 loopback, and the cloud gateway reaches the allocated internal port through the host tunnel.

## Project and host storage

Users choose where setup writes the file:

- `.clank/launch.yaml` is shareable. It works for teammates and cloud worktrees after it is committed and pushed.
- The host-only file lives under Clank's config directory and is keyed by the main Git worktree identity. It is useful for private preferences and persists in Clank's persistent sandboxes. Linked worktrees share it.

The project file takes precedence. If it exists but is invalid, Clank reports the error and does not fall through to the host file. This makes the effective configuration unambiguous.

Clank may use `.claude/launch.json` as evidence during agent setup, but never imports it at runtime or keeps it synchronized. The generated Clank file is independent from then on.

## Setup responsibilities

The generated agent task requires the agent to:

1. inspect documentation, package scripts, lockfiles, Makefiles, and monorepo structure;
2. ask the user to choose project or host storage;
3. avoid Docker and Podman, which Clank does not currently provide;
4. use immutable or frozen dependency installation when installation is part of setup;
5. start every entry with a temporary `PORT`, verify readiness, stop it, and correct failures; and
6. avoid committing, pushing, or opening a pull request without explicit permission.

Setup may define several names, but v1 intentionally does not accept a positional subdirectory for managed launch. The configured name supplies both the launch directory and service identity, while the repository root remains the agent's project context.

## Trust boundary

A launch command is executable project configuration. Starting a preview runs it with the same user permissions as `clank-host`. Review changes to a tracked `.clank/launch.yaml` with the same care as package scripts, Makefiles, or other development tooling. Host-only configuration is writable only through the user's Clank state directory.
