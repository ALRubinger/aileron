#!/usr/bin/env bash
#
# Orchestrates the tier-2 (real-daemon) Playwright integration suite.
#
# - Builds the daemon (or assumes a fresh `build/aileron-server` from task deps).
# - Creates an isolated state dir + vault file under a temporary HOME.
# - Starts the daemon with the basic.json seed fixture on an ephemeral
#   loopback port.
# - Waits for the daemon to publish its URL via daemon.json.
# - Runs Playwright with AILERON_WEBAPP_URL pointed at the daemon.
# - Tears the daemon down on any exit path (success, failure, signal).
#
# Run via `task test:webapp:e2e:integration` from the repo root.

set -euo pipefail

# Resolve repo root from this script's location, so the target works
# regardless of where Task chose to run it.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SERVER_BIN="${REPO_ROOT}/build/aileron-server"
AILERON_BIN="${REPO_ROOT}/build/aileron"
SEED_FILE="${REPO_ROOT}/test/fixtures/action-approvals/basic.json"
WEBAPP_DIR="${REPO_ROOT}/webapp"

if [[ ! -x "${SERVER_BIN}" ]]; then
  echo "error: ${SERVER_BIN} not found — run 'task build:server' first" >&2
  exit 1
fi
if [[ ! -x "${AILERON_BIN}" ]]; then
  echo "error: ${AILERON_BIN} not found — run 'task build:cli' first" >&2
  exit 1
fi
if [[ ! -f "${SEED_FILE}" ]]; then
  echo "error: seed fixture missing at ${SEED_FILE}" >&2
  exit 1
fi

TMP_HOME="$(mktemp -d -t aileron-e2e-XXXXXX)"
mkdir -p "${TMP_HOME}/.aileron"

cleanup() {
  if [[ -n "${DAEMON_PID:-}" ]] && kill -0 "${DAEMON_PID}" 2>/dev/null; then
    kill "${DAEMON_PID}" 2>/dev/null || true
    # Give the daemon up to 5s to shut down cleanly.
    for _ in $(seq 1 25); do
      if ! kill -0 "${DAEMON_PID}" 2>/dev/null; then
        break
      fi
      sleep 0.2
    done
    kill -9 "${DAEMON_PID}" 2>/dev/null || true
  fi
  rm -rf "${TMP_HOME}"
}
trap cleanup EXIT INT TERM

# Initialize a vault under TMP_HOME so the daemon can start. The
# passphrase doesn't matter — none of the approvals endpoints need to
# read credentials, so the daemon stays vault-locked for the duration
# of the run.
HOME="${TMP_HOME}" AILERON_VAULT_PASSPHRASE=test-passphrase \
  "${AILERON_BIN}" vault init >/dev/null

# Start the daemon. --bind 127.0.0.1:0 picks an ephemeral port; the
# daemon writes the chosen URL to daemon.json which we read below.
HOME="${TMP_HOME}" "${SERVER_BIN}" \
  --bind 127.0.0.1:0 \
  --dev-seed-approvals "${SEED_FILE}" \
  >"${TMP_HOME}/server.log" 2>&1 &
DAEMON_PID=$!

# Poll daemon.json for up to 10s waiting for the URL and bearer token to appear.
DAEMON_URL=""
DAEMON_TOKEN=""
for _ in $(seq 1 50); do
  if [[ -f "${TMP_HOME}/.aileron/daemon.json" ]]; then
    DAEMON_URL="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["url"])' "${TMP_HOME}/.aileron/daemon.json" 2>/dev/null || true)"
    DAEMON_TOKEN="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("token", ""))' "${TMP_HOME}/.aileron/daemon.json" 2>/dev/null || true)"
    if [[ -n "${DAEMON_URL}" && -n "${DAEMON_TOKEN}" ]]; then
      break
    fi
  fi
  sleep 0.2
done
if [[ -z "${DAEMON_URL}" || -z "${DAEMON_TOKEN}" ]]; then
  echo "error: daemon did not publish daemon.json URL and token within 10s" >&2
  echo "--- daemon log ---" >&2
  cat "${TMP_HOME}/server.log" >&2 || true
  exit 1
fi

# Confirm reachability before handing off to Playwright.
AUTH_HEADER="Authorization: Bearer ${DAEMON_TOKEN}"
if ! curl -fsS -H "${AUTH_HEADER}" "${DAEMON_URL}/v1/action-approvals" >/dev/null; then
  echo "error: daemon at ${DAEMON_URL} not reachable" >&2
  echo "--- daemon log ---" >&2
  cat "${TMP_HOME}/server.log" >&2 || true
  exit 1
fi

# Unlock the vault. Otherwise the webapp's layout opens the passphrase
# modal on every page load, and the modal's full-screen overlay
# intercepts clicks on the approval cards. The approvals endpoints
# themselves don't read credentials, so this is purely a UI-layer
# concern — but Playwright sees real DOM, so we have to clear it.
if ! curl -fsS -X POST -H 'Content-Type: application/json' -H "${AUTH_HEADER}" \
  -d '{"passphrase":"test-passphrase"}' \
  "${DAEMON_URL}/v1/vault/unlock" >/dev/null; then
  echo "error: vault unlock failed" >&2
  echo "--- daemon log ---" >&2
  cat "${TMP_HOME}/server.log" >&2 || true
  exit 1
fi

echo "daemon listening at ${DAEMON_URL} (pid ${DAEMON_PID})"

cd "${WEBAPP_DIR}"
AILERON_WEBAPP_URL="${DAEMON_URL}" \
  pnpm exec playwright test --config=playwright.integration.config.ts "$@"
