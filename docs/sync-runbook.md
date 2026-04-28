# Hub-to-Hub Sync Runbook (MVP / Goalpost 1)

This runbook drives the bidirectional sync MVP end-to-end on a single
laptop. You run two `clankd` instances simultaneously: one acts as the
laptop hub, the other as the cloud hub. The cloud hub receives
synced git data from the laptop and provisions Daytona sandboxes (or,
with the `local-stub` launcher, in-process clank-hosts) that clone
their working tree from the cloud mirror.

There is **no LLM call** in this runbook — it stops at "session
created, workdir resolved." Adding a real prompt requires
`ANTHROPIC_API_KEY` (or equivalent) on the laptop and is the next
manual step you can take.

---

## Prerequisites

- A working clank repo: `make install` builds `clank`, `clankd`,
  `clank-host` into `$GOPATH/bin`.
- `git` 2.40+ on both ends (smart-HTTP backend uses `git http-backend`).
- A real local git repo to opt into syncing (any will do — pick one
  whose `origin` is set).
- For the Daytona path: a Daytona API key and the published image
  (`ghcr.io/acksell/clank-host:<tag>` — see `cmd/clank-host/Dockerfile`).
  For the dev/CI path: the `local-stub` launcher works with no extra
  setup.

---

## Step 1 — Configure the cloud (remote) hub

```bash
export CLANK_DIR=~/.clank-cloud
mkdir -p "$CLANK_DIR"
```

Create `$CLANK_DIR/preferences.json`:

```json
{
  "remote_hub": {
    "auth_token": "test-token-please-change"
  },
  "daytona": {
    "api_key": "<DAYTONA_API_KEY>",
    "extra_env": {
      "ANTHROPIC_API_KEY": "<your-key>"
    }
  }
}
```

The `daytona` block is optional. Omit it for the local-stub demo.

Start the cloud hub:

```bash
CLANK_DIR=~/.clank-cloud clankd start --foreground \
  --listen tcp://127.0.0.1:7878 \
  --public-base-url http://127.0.0.1:7878
```

Sanity check: 401 without a token, 200 with the right one.

```bash
curl http://127.0.0.1:7878/ping                 # → 401
curl -H "Authorization: Bearer test-token-please-change" \
     http://127.0.0.1:7878/ping                 # → 200 + JSON
```

---

## Step 2 — Configure the laptop hub

```bash
export CLANK_DIR=~/.clank-laptop
mkdir -p "$CLANK_DIR"
```

Create `$CLANK_DIR/preferences.json`:

```json
{
  "remote_hub": {
    "url": "http://127.0.0.1:7878",
    "auth_token": "test-token-please-change"
  },
  "synced_repos": [
    "/Users/you/path/to/your-repo"
  ]
}
```

The `synced_repos` list is **local paths**, one per line. The agent
reads each path's `origin` URL at sync time — repos with no origin
are skipped with a warning.

Start the laptop hub (default Unix socket):

```bash
CLANK_DIR=~/.clank-laptop clankd start --foreground
```

You should see `sync agent: pushing to http://127.0.0.1:7878 for 1 repo(s)` in the logs.

---

## Step 3 — Push a bundle (automatic)

In your local repo, create a branch and commit a unique marker file:

```bash
cd ~/path/to/your-repo
git checkout -b feat/sync-runbook
echo "synced via clank — $(date)" > marker.txt
git add marker.txt
git commit -m "runbook marker"
```

Within ~5 seconds the laptop sync agent picks up the new tip and pushes
a bundle to the cloud hub. Watch the cloud hub's logs:

```
sync: received <repo_key>/feat/sync-runbook tip=<sha> base=
```

Verify via the public API:

```bash
curl -H "Authorization: Bearer test-token-please-change" \
     http://127.0.0.1:7878/sync/repos | jq
```

You should see your repo with a `feat/sync-runbook` branch and the
matching tip SHA.

---

## Step 4 — Verify the smart-HTTP mirror is clonable

Cloud sandboxes will use this exact path. Confirm it works first:

```bash
git -c http.extraHeader="Authorization: Bearer test-token-please-change" \
    clone -b feat/sync-runbook \
    http://127.0.0.1:7878/sync/repos/<repo_key>/git \
    /tmp/clank-mirror-check
ls /tmp/clank-mirror-check        # marker.txt should be there
```

Replace `<repo_key>` with the value from Step 3's `/sync/repos` listing.

---

## Step 5 — Trigger a session via the `local-stub` launcher

This is the dev path that doesn't require Daytona. The cloud hub
spawns a fresh `clank-host` on a TCP port and registers it as a
just-another-host in its catalog. The spawned host clones from the
cloud mirror at session time.

```bash
curl -H "Authorization: Bearer test-token-please-change" \
     -H "Content-Type: application/json" \
     -X POST http://127.0.0.1:7878/sessions \
     -d '{
       "git_ref": {
         "remote_url": "<your repo origin URL>",
         "worktree_branch": "feat/sync-runbook"
       },
       "prompt": "list files in this repo and read marker.txt",
       "backend": "claude-code",
       "launch_host": {"provider": "local-stub"}
     }'
```

Cloud hub log lines you should see, in order:

```
local launcher: spawned host local-XXXXXXXX at http://127.0.0.1:NNNN
[clank-host:tcp://127.0.0.1:NNNN] [clank-host] listening on tcp://127.0.0.1:NNNN
[clank-host:tcp://127.0.0.1:NNNN] [clank-host] cloning http://127.0.0.1:7878/sync/repos/<repo_key>/git into ...
```

If the spawned host has the agent backend installed and credentials in
its env, the session reaches `Status=idle` after the first reply.
Without those, you'll see a backend-creation error in the logs — the
sync path itself is still validated by the successful clone line.

---

## Step 6 — Trigger a session via Daytona

Same request, with `"provider": "daytona"`:

```bash
curl -H "Authorization: Bearer test-token-please-change" \
     -H "Content-Type: application/json" \
     -X POST http://127.0.0.1:7878/sessions \
     -d '{
       "git_ref": {
         "remote_url": "<your repo origin URL>",
         "worktree_branch": "feat/sync-runbook"
       },
       "prompt": "list files in this repo and read marker.txt",
       "backend": "claude-code",
       "launch_host": {"provider": "daytona"}
     }'
```

The cloud hub:

1. Calls `POST https://app.daytona.io/api/sandbox` with the
   pre-baked image and your env (including `CLANK_HUB_URL`,
   `CLANK_HUB_TOKEN`, and any `daytona.extra_env` from preferences).
2. Polls until the sandbox state is `Running`.
3. Fetches the preview URL + `x-daytona-preview-token` for the
   sandbox's port 7878.
4. Registers the sandbox as a host (`daytona-<uuid>`) with a
   transport that injects the preview token on every request.
5. Dispatches the session there.

Daytona accrues cost while sandboxes run; clankd's
`Launcher.Stop()` is wired into the shutdown path and DELETEs every
sandbox it created. SIGTERM the cloud hub when you're done.

---

## Step 7 — Subscribe to events (mobile would do this)

```bash
curl -N -H "Authorization: Bearer test-token-please-change" \
     http://127.0.0.1:7878/events
```

You'll see SSE frames for `session_create`, `status_change`,
`message`, `part`, etc. — same format the TUI consumes today.

---

## What's NOT in this runbook (deferred to later phases)

- **Dirty / uncommitted changes**: Phase 1. Today the agent pushes
  only committed branch tips. Add a stash-bundle step in the agent.
- **Pulling cloud sessions to the laptop TUI**: Phase 4. Today the
  laptop has no view into the cloud hub's session list.
- **Live transfer of a running session**: Phase 3. Today only fresh
  sessions land in the cloud.
- **Mobile UI**: out of repo. The cloud hub's HTTP API is the
  contract; build a client in any language.
- **TLS / certificate auth**: today's bearer token is a single
  pre-shared static value. Rotation, JWTs, mTLS are listener-level
  changes, not protocol changes.
- **Bundle GC**: incoming/ files are cleaned up but the bare mirror
  grows monotonically. A `git gc --auto` cron is the obvious next step.

---

## Troubleshooting

- **"address already in use"** on the cloud hub: another process is
  bound to the port. Pick a different one with `--listen tcp://...`.
- **`/ping` returns 401 even with the right token**: check that the
  `Authorization` header is exactly `Bearer <token>`, no trailing
  newline.
- **Bundle pushes never arrive**: verify (a) the laptop hub is using
  the laptop's `CLANK_DIR` (not the cloud's), (b) the synced repo
  path is absolute, (c) the repo has an `origin` remote.
- **`local-stub` clone fails with auth error**: the cloud hub's
  `--public-base-url` must be reachable from the spawned `clank-host`.
  On a single laptop, `http://127.0.0.1:7878` works because all
  processes share the loopback interface.
- **Daytona sandbox stuck in `Pending`**: usually image pull issues.
  Daytona's dashboard surfaces logs; `provision_timeout` (default 90s)
  trips the launcher to delete the sandbox and bubble the error up.
