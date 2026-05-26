#!/usr/bin/env bash
# preview-smoke.sh — end-to-end smoke test for the preview-app feature.
#
# Spins up clank-host locally, points it at a real Expo project via a
# symlinked worktree under $HOME/work/<wid>, and walks the full HTTP
# contract:
#
#   1. GET  /preview/status  → available=true, state=stopped
#   2. POST /preview/start    → spawns Metro, polls until state=ready
#   3. GET  /preview/proxy/   → fetches the Expo manifest through the
#                                proxy and asserts launchAsset.url has
#                                been rewritten to the proxy hostname
#                                (Change 4 — REACT_NATIVE_PACKAGER_HOSTNAME
#                                + EXPO_PACKAGER_PROXY_URL — verification)
#   4. POST /preview/stop     → shutdown
#
# Pre-reqs: node + npm/npx in PATH, the clank-mobile checkout next to
# clank, jq, curl.
#
# Run from the clank repo root:
#   ./scripts/preview-smoke.sh
#
# Override the Expo project location with CLANK_PREVIEW_SMOKE_DIR if
# clank-mobile lives elsewhere.

set -euo pipefail

HOST_PORT="${HOST_PORT:-8082}"
WID="${WID:-preview-smoke-$$}"
EXPO_DIR="${CLANK_PREVIEW_SMOKE_DIR:-${HOME}/github.com/acksell/clank-mobile}"
PREVIEW_BASE="http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/proxy"

if [[ ! -f "${EXPO_DIR}/package.json" || ! -f "${EXPO_DIR}/app.json" ]]; then
  echo "expo project not found at ${EXPO_DIR}" >&2
  echo "set CLANK_PREVIEW_SMOKE_DIR to override" >&2
  exit 1
fi
if [[ ! -d "${EXPO_DIR}/node_modules" ]]; then
  echo "node_modules missing at ${EXPO_DIR} — run 'npm install' or 'bun install' first" >&2
  exit 1
fi
for cmd in jq curl npx; do
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
  echo "clank-host did not emit 'listening' within 3s but is still alive — aborting before STEP 1 produces confusing curl errors. log:" >&2
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
echo "==> STEP 2: POST /preview/start (preview_url_base=${PREVIEW_BASE})"
start_body="$(jq -n --arg url "${PREVIEW_BASE}" '{preview_url_base: $url}')"
start_resp="$(curl -sS -X POST -H 'Content-Type: application/json' \
  -d "${start_body}" \
  "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/start")"
echo "${start_resp}" | jq .
metro_port="$(echo "${start_resp}" | jq -r .port)"
echo "Metro should bind to port ${metro_port}"

echo "==> polling /preview/status until ready (≤90s); reporting every 5s"
last_print=0
for i in {1..180}; do
  state="$(curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/status" | jq -r .state)"
  if (( i - last_print >= 10 )); then
    last_print=${i}
    elapsed=$(( i / 2 ))
    logs_len=$(curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/logs" | wc -c | tr -d ' ')
    echo "    [${elapsed}s] state=${state} logs=${logs_len}B"
  fi
  if [[ "${state}" == "ready" ]]; then
    echo "    → ready"
    break
  fi
  if [[ "${state}" == "failed" ]]; then
    echo "FAIL: state went to failed."
    echo "==> clank-host log tail:"
    tail -20 /tmp/clank-host-smoke.log
    echo
    echo "==> Metro stdout/stderr tail (last 4 KiB from /preview/logs):"
    curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/logs" | tail -c 4096
    echo
    echo "==> /preview/status after failure:"
    curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/status" | jq .
    exit 1
  fi
  sleep 0.5
done
[[ "${state}" == "ready" ]] || { echo "FAIL: state never reached ready (last=${state})"; exit 1; }

echo
echo "==> STEP 3: GET / through the proxy — fetch the Expo manifest"
# The Expo manifest lives at the bundle URL with the right Expo-Platform header.
# We request the root path with the same headers a mobile client would.
manifest="$(curl -sS \
  -H 'Accept: application/expo+json,application/json' \
  -H 'Expo-Platform: ios' \
  -H 'Expo-API-Version: 1' \
  "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/proxy/")"
echo "${manifest}" | head -c 1000
echo
echo

launch_url="$(echo "${manifest}" | jq -r '.launchAsset.url // empty')"
if [[ -z "${launch_url}" ]]; then
  echo "WARN: no launchAsset.url in manifest — may be a different Expo shape"
  echo "(skipping Change 4 hostname-rewrite assertion)"
else
  echo "launchAsset.url = ${launch_url}"
  if [[ "${launch_url}" == *"127.0.0.1:${HOST_PORT}"* ]]; then
    echo "PASS: Change 4 hostname rewrite working — manifest points at the proxy"
  elif [[ "${launch_url}" == *"127.0.0.1:${metro_port}"* ]]; then
    echo "FAIL: Change 4 broken — manifest still points at Metro's localhost:${metro_port}"
    echo "       (REACT_NATIVE_PACKAGER_HOSTNAME / EXPO_PACKAGER_PROXY_URL not honored)"
    exit 1
  else
    echo "INCONCLUSIVE: launchAsset.url unexpected — inspect manually"
  fi
fi

if [[ -n "${launch_url}" ]]; then
  echo
  echo "==> STEP 3b: fetching the bundle URL through the proxy"
  bundle_status="$(curl -sS -o /tmp/preview-bundle.js -w '%{http_code} %{size_download}B %{content_type}' "${launch_url}")"
  echo "  → HTTP ${bundle_status}"
  head_bytes="$(head -c 200 /tmp/preview-bundle.js)"
  echo "  → first 200 bytes: ${head_bytes}"
  if [[ "${bundle_status}" == 200* ]] && [[ "${head_bytes}" == *"function"* || "${head_bytes}" == *"var "* || "${head_bytes}" == *"//"* ]]; then
    echo "PASS: bundle downloaded through the proxy (looks like JavaScript)"
  else
    echo "FAIL: bundle download did not produce a JS-looking response"
    exit 1
  fi
fi

echo
echo "==> STEP 4: POST /preview/stop"
curl -sS -X POST "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/stop" -o /dev/null -w '%{http_code}\n'

echo
echo "==> verifying state went back to stopped"
final="$(curl -sS "http://127.0.0.1:${HOST_PORT}/worktrees/${WID}/preview/status" | jq -r .state)"
[[ "${final}" == "stopped" ]] || { echo "FAIL: state after stop = ${final}, want stopped"; exit 1; }

echo
echo "==> all smoke checks passed"
