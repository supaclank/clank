#!/usr/bin/env bash
# preview-smoke.sh — local smoke test for the post-rewriter preview path.
#
# Drives the sprite-side preview lifecycle without involving a real
# gateway. clank-host runs locally, spawns Metro for a symlinked
# Expo worktree, and we walk the trimmed HTTP contract:
#
#   1. GET  /preview/status  → available=true, state=stopped
#   2. POST /preview/start   → spawns Metro, polls until state=ready
#   3. POST /preview/stop    → shutdown, state=stopped
#
# What this script no longer asserts (vs V1):
#
#   - "manifest URL rewriting" — clank-host doesn't proxy preview
#     traffic anymore. The gateway tunnels straight to Metro via
#     Sprites' WSS proxy and Metro emits manifest URLs from r.Host
#     directly. End-to-end fidelity of that path is exercised by
#     the gateway preview_proxy_test.go (WS upgrade through tunnel)
#     and ultimately verified by a separate cloud-side smoke once
#     the gateway is provisioned with a wildcard cert.
#
# Pre-reqs: node + bun in PATH (bun launches the local expo bin, Metro
# runs under node), a real Expo project at CLANK_PREVIEW_SMOKE_DIR
# (env var, required), jq, curl.

set -euo pipefail

HOST_PORT="${HOST_PORT:-8082}"
WID="${WID:-preview-smoke-$$}"
EXPO_DIR="${CLANK_PREVIEW_SMOKE_DIR:-}"

if [[ -z "${EXPO_DIR}" ]]; then
  echo "CLANK_PREVIEW_SMOKE_DIR is required — point it at an Expo project root" >&2
  exit 1
fi

if [[ ! -f "${EXPO_DIR}/package.json" || ! -f "${EXPO_DIR}/app.json" ]]; then
  echo "expo project not found at ${EXPO_DIR}" >&2
  echo "set CLANK_PREVIEW_SMOKE_DIR to override" >&2
  exit 1
fi
if [[ ! -d "${EXPO_DIR}/node_modules" ]]; then
  echo "node_modules missing at ${EXPO_DIR} — run 'bun install' first" >&2
  exit 1
fi
for cmd in jq curl bun node; do
  command -v "${cmd}" >/dev/null 2>&1 || { echo "missing: ${cmd}" >&2; exit 1; }
done

WORK_LINK="${HOME}/work/${WID}"
mkdir -p "${HOME}/work"
ln -sfn "${EXPO_DIR}" "${WORK_LINK}"

HOST_PID=""
cleanup() {
  set +e
  if [[ -n "${HOST_PID}" ]] && kill -0 "${HOST_PID}" 2>/dev/null; then
    echo "==> cleanup: stopping clank-host (pid ${HOST_PID})"
    kill "${HOST_PID}" 2>/dev/null
    wait "${HOST_PID}" 2>/dev/null
  fi
  if [[ -L "${WORK_LINK}" ]]; then
    rm "${WORK_LINK}"
  fi
}
trap cleanup EXIT INT TERM

echo "==> building clank-host"
go build -o /tmp/clank-host-smoke ./cmd/clank-host

echo "==> starting clank-host on 127.0.0.1:${HOST_PORT}"
# No --preview-webhook-url: gwclient stays disabled, Status.token/url
# will be empty. That's deliberate for the local smoke — verifying the
# gateway integration belongs in the cloud-side smoke once Phase 0's
# infra (wildcard cert + DNS) is in place.
/tmp/clank-host-smoke --listen "tcp://127.0.0.1:${HOST_PORT}" >/tmp/clank-host-smoke.log 2>&1 &
HOST_PID=$!

# Wait for the listen line.
for i in {1..30}; do
  if grep -q "listening on tcp://" /tmp/clank-host-smoke.log 2>/dev/null; then
    break
  fi
  if ! kill -0 "${HOST_PID}" 2>/dev/null; then
    echo "clank-host died early. log:" >&2
    cat /tmp/clank-host-smoke.log >&2
    exit 1
  fi
  sleep 0.1
done

if ! grep -q "listening on tcp://" /tmp/clank-host-smoke.log 2>/dev/null; then
  echo "clank-host did not emit 'listening' within 3s but is still alive — aborting." >&2
  cat /tmp/clank-host-smoke.log >&2
  exit 1
fi

echo
echo "==> STEP 1: GET /preview/status (expect Available=true, State=stopped)"
status_json="$(curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/status")"
echo "${status_json}" | jq .
[[ "$(echo "${status_json}" | jq -r .available)" == "true" ]] || { echo "FAIL: available should be true"; exit 1; }
[[ "$(echo "${status_json}" | jq -r .kind)" == "expo" ]]      || { echo "FAIL: kind should be expo"; exit 1; }
[[ "$(echo "${status_json}" | jq -r .state)" == "stopped" ]]  || { echo "FAIL: state should be stopped"; exit 1; }

echo
echo "==> STEP 2: POST /preview/start (no body — gateway mints URL)"
start_resp="$(curl -sS -X POST -H 'Content-Type: application/json' \
  -d '' \
  "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/start")"
echo "${start_resp}" | jq .
state="$(echo "${start_resp}" | jq -r .state)"
metro_port="$(echo "${start_resp}" | jq -r .port)"
token="$(echo "${start_resp}" | jq -r '.token // ""')"
url="$(echo "${start_resp}" | jq -r '.url // ""')"
echo "Metro bound to port ${metro_port}; state=${state}"
if [[ -n "${token}" ]]; then
  echo "Token minted: ${token} → ${url}"
else
  echo "(no gateway wired — token/url empty, as expected for local smoke)"
fi

[[ "${state}" == "ready" ]] || { echo "FAIL: start should return state=ready (it now blocks on readiness); got ${state}"; exit 1; }

echo
echo "==> STEP 3: confirm Metro is actually serving on the internal port"
# Direct probe — the gateway would do this via WSS tunnel in
# production. We hit it locally to make sure Metro is up.
probe_status="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${metro_port}/status" || echo "000")"
if [[ "${probe_status}" == "200" ]]; then
  echo "PASS: Metro is serving /status on 127.0.0.1:${metro_port}"
else
  echo "FAIL: /status probe to Metro returned ${probe_status}"
  exit 1
fi

echo
echo "==> STEP 4: confirm Metro stays in watch mode (NOT 'CI mode')"
# Direct regression test for the spawn.buildEnv bug we hit during
# initial HMR bring-up: setting CI=true in the child env makes Metro
# disable file-watching + HMR with the line below. Targeted argv
# flag (--non-interactive) is the supported way to silence prompts.
#
# Metro's stdout lives in clank-host's in-memory ring buffer, not the
# host process stdout — so we fetch via /preview/logs.
metro_logs="$(
  curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/logs"
)"
if echo "${metro_logs}" | grep -q "Metro is running in CI mode"; then
  echo "FAIL: Metro reports 'CI mode' — HMR is silently broken."
  echo "      Check spawn.buildEnv: nothing should set CI in the child env."
  echo "${metro_logs}" | grep -n "Metro is running in CI mode" | head -3 >&2
  exit 1
fi
echo "PASS: Metro's logs don't contain 'Metro is running in CI mode'"

echo
echo "==> STEP 5: manifest's launchAsset.url has the expected host:port"
# Metro generates the bundle URL from EXPO_PACKAGER_PROXY_URL if set,
# otherwise its own listen host:port. The local smoke runs without a
# gateway → no proxy URL → manifest should advertise Metro's internal
# port. The flip side (with proxy URL → manifest reflects it) is
# pinned in internal/host/preview/spawn_test.go.
#
# This step exists to catch a *Metro* regression: if a future SDK
# changes how launchAsset.url is composed, this catches it before
# users do. The contract we assert: launchAsset.url's port equals
# Metro's bound port (the spawn returned ${metro_port}).
manifest_json="$(
  curl -sS \
    -H 'Accept: application/expo+json,application/json' \
    -H 'expo-platform: android' \
    "http://127.0.0.1:${metro_port}/" \
)"
launch_asset_url="$(echo "${manifest_json}" | jq -r '.launchAsset.url // empty')"
if [[ -z "${launch_asset_url}" ]]; then
  echo "FAIL: manifest has no .launchAsset.url"
  echo "${manifest_json}" | jq . >&2 || echo "${manifest_json}" >&2
  exit 1
fi
echo "launchAsset.url: ${launch_asset_url}"
# Extract host:port — works for the http://host:port/path form.
manifest_hostport="$(echo "${launch_asset_url}" | sed -E 's|^https?://([^/]+)/.*|\1|')"
expected_hostport="127.0.0.1:${metro_port}"
if [[ "${manifest_hostport}" != "${expected_hostport}" ]]; then
  echo "FAIL: launchAsset.url host:port = '${manifest_hostport}', want '${expected_hostport}'"
  echo "      Metro is composing manifest URLs from an unexpected source."
  exit 1
fi
echo "PASS: launchAsset.url advertises ${expected_hostport}"

echo
echo "==> STEP 6: POST /preview/stop"
stop_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/stop")
echo "${stop_status}"
[[ "${stop_status}" -eq 200 ]] || { echo "FAIL: stop returned HTTP ${stop_status}"; exit 1; }

echo
echo "==> verifying state went back to stopped"
final="$(curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/status" | jq -r .state)"
[[ "${final}" == "stopped" ]] || { echo "FAIL: state after stop = ${final}, want stopped"; exit 1; }

echo
echo "==> all smoke checks passed"
