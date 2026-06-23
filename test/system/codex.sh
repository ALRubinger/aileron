#!/bin/sh
# test/system/codex.sh
#
# The codex `aileron launch` system-test scenario body (#1476). Invoked by
# the Taskfile target test:system:launch:codex, which owns the reusable
# build seam, the fail-fast preconditions, and the `defer` cleanup (#1475);
# this script owns only the codex-specific scenario logic and reuses the
# shared R8 probes in test/system/lib/probes.sh verbatim.
#
# It NEVER runs `docker run` directly: it drives `aileron launch codex -- ...`
# so the launcher orchestrates the image pull + container run, then inspects
# the resulting container out-of-band by its stable name prefix.
#
# Required environment (exported by the Taskfile target):
#   AILERON_BIN          absolute path to the freshly built aileron binary
#   AILERON_SYSTEST_LIB  absolute path to test/system/lib
#   WORKSPACE            temp workspace dir bind-mounted into the container
#   AILERON_STATE_DIR    daemon state dir (for the R10 audit read); optional
#
# Exit code 0 means every assertion passed; non-zero aborts and the
# Taskfile's deferred cleanup still removes the container + workspace.
#
# STATIC GATE: this script performs a live `aileron launch codex`, which
# needs a real OpenAI/Codex login and consumes LLM tokens. It is run BY HAND
# on a real host. The headless CI/agent path validates the harness with
# `task --dry test:system:launch:codex`, which compiles the target without
# executing this script. See test/system/README.md.

set -eu

# --- codex-specific parameters (passed to the shared probes) ---------------
AGENT='codex'
IMAGE_SUFFIX='codex'
CONFIG_PATH='/home/agent/.codex/config.toml'
AUTH_PATH='/home/agent/.codex/auth.json'
MCP_MARKER='[mcp_servers.aileron]'
WORKSPACE_CONTAINER_PATH='/home/agent/workspace'
SENTINEL_NAME='.aileron-systest-sentinel'

# Image tag: a dev/empty version build pulls :edge, a release pulls :latest
# (internal/sandbox/composition/composition.go imageTag). The system test
# targets dev builds, so :edge is the default; override with EXPECTED_IMAGE
# to assert a release image or a digest pin.
EXPECTED_IMAGE="${EXPECTED_IMAGE:-ghcr.io/alrubinger/aileron-sandbox-${IMAGE_SUFFIX}:edge}"

: "${AILERON_BIN:?AILERON_BIN must point at the built aileron binary}"
: "${AILERON_SYSTEST_LIB:?AILERON_SYSTEST_LIB must point at test/system/lib}"
: "${WORKSPACE:?WORKSPACE must be a writable temp dir}"

# shellcheck source=test/system/lib/assert.sh
. "$AILERON_SYSTEST_LIB/assert.sh"
# shellcheck source=test/system/lib/probes.sh
. "$AILERON_SYSTEST_LIB/probes.sh"

# --- per-run sentinel (R9): a fresh token so a stale file can't pass --------
RUNID="$(date +%s)-$$"
SENTINEL_EXPECTED="AILERON_SYSTEST_OK_${RUNID}"

# The agent prompt drives three deterministic, host-verifiable side effects:
#  R9  write the exact sentinel string into the mounted workspace, and
#  R10 invoke the built-in http_request MCP tool against the daemon's own
#      health endpoint so a daemon-side audit record lands for this run.
# The instruction is explicit and self-contained so the agent need not
# improvise; the verification reads the workspace file + audit log, never the
# agent's prose.
EXEC_PROMPT="Do exactly two things and then stop. \
1) Write the exact text ${SENTINEL_EXPECTED} (no newline, nothing else) to the file ${WORKSPACE_CONTAINER_PATH}/${SENTINEL_NAME}. \
2) Call the aileron http_request tool with method GET and url \${AILERON_URL}/healthz."

log "codex scenario: runid=${RUNID} workspace=${WORKSPACE} image=${EXPECTED_IMAGE}"

# --- launch (R7a arg-forwarding asserted: post-\`--\` tokens reach codex) -----
# `-- exec "<prompt>"` forwards verbatim through LaunchConfig.Args
# (cmd/aileron/main.go applyTrailingLaunchFlags) ->
# launcher.go `command = append(command, config.Args...)` ->
# runtime.go image then opts.Command. We run the launch in the background so
# the live-container probes can inspect the container while codex is still
# resident, then reap the exit code.
LAUNCH_LOG="$WORKSPACE/.launch.log"
(
	cd "$WORKSPACE"
	# --sandbox=docker is the codex default (cmd/aileron/main.go), so we do
	# not pass an explicit sandbox flag — the run also asserts that default.
	"$AILERON_BIN" launch "$AGENT" -- exec "$EXEC_PROMPT"
) >"$LAUNCH_LOG" 2>&1 &
LAUNCH_PID=$!

# Under `set -e` a failing probe aborts the script before the explicit
# `wait` below, which would orphan the backgrounded launch (and its sandbox
# container, whose runtime name we cannot predict so the Taskfile cleanup
# can't target it). Reap the launch on ANY exit so a mid-probe failure tears
# the launch down; killing the `aileron launch` process stops its
# `docker run --rm` container too. Idempotent with the explicit wait.
reap_launch() {
	if kill -0 "$LAUNCH_PID" 2>/dev/null; then
		kill "$LAUNCH_PID" 2>/dev/null || true
		wait "$LAUNCH_PID" 2>/dev/null || true
	fi
}
trap reap_launch EXIT INT TERM

# --- wait for the container, then run the live-container R8 probes ----------
container=''
i=0
while [ "$i" -lt 120 ]; do
	if ! kill -0 "$LAUNCH_PID" 2>/dev/null; then
		# Launch exited before we saw a container — short-lived run; the
		# post-run assertions below still apply.
		log "launch exited before container probe window; continuing to post-run checks"
		break
	fi
	if container="$(discover_container "$AILERON_SBX_PREFIX")"; then
		break
	fi
	container=''
	i=$((i + 1))
	sleep 1
done

if [ -n "$container" ]; then
	log "probing running container: $container"
	probe_image "$container" "$EXPECTED_IMAGE"
	probe_mcp "$container" "$CONFIG_PATH" "$MCP_MARKER"
	probe_credentials "$container" "$AUTH_PATH"
	probe_daemon_reachable "$container"
else
	fail "never observed a running ${AILERON_SBX_PREFIX}* container during the launch"
fi

# --- reap the launch and assert its exit code (R8.6) -----------------------
LAUNCH_EXIT=0
wait "$LAUNCH_PID" || LAUNCH_EXIT=$?
probe_exit_code "$LAUNCH_EXIT" 0 ||
	{ log "launch log follows:"; cat "$LAUNCH_LOG" >&2; exit 1; }

# --- R8.5 clean teardown: container is gone after `docker run --rm` ---------
probe_teardown "$AILERON_SBX_PREFIX"

# --- R9 deterministic result: read the sentinel from the host side ---------
SENTINEL_HOST="$WORKSPACE/$SENTINEL_NAME"
if [ ! -f "$SENTINEL_HOST" ]; then
	fail "R9 sentinel file not found at $SENTINEL_HOST (agent did not write it)"
	exit 1
fi
# Read byte-exact; trim a single trailing newline the editor/agent may add so
# the comparison is robust without accepting arbitrary trailing content.
SENTINEL_ACTUAL="$(cat "$SENTINEL_HOST")"
assert_eq "$SENTINEL_EXPECTED" "$SENTINEL_ACTUAL" "R9 sentinel content byte-exact"

# --- R7a forwarding: the agent acted on the forwarded `exec` instruction ----
# The sentinel above is itself the proof that post-\`--\` args reached the
# codex CLI: codex only ran our exec prompt if `-- exec "<prompt>"` was
# forwarded into the container command. Assert explicitly for a clear signal.
assert_not_empty "$SENTINEL_ACTUAL" "R7a post-\`--\` exec args forwarded to codex"

# --- R10 round-trip audit (authored; deterministic where the daemon logs) --
# The built-in http_request tool routes through the daemon's /comms/http
# handler, which appends a message audit entry to today's audit JSONL
# (internal/audit/local.go MessageEntry; written by handlers_comms.go
# logCommsEvent). We jq-assert that today's audit file contains a record for
# this session within the run window. When the daemon's OTel tracing is
# enabled, the richer span record (aileron.action.name) also lands under
# <stateDir>/traces/spans-YYYY-MM-DD.jsonl; that path is documented in the
# README as the tracing-gated variant.
if [ -n "${AILERON_STATE_DIR:-}" ]; then
	AUDIT_FILE="${AILERON_STATE_DIR}/audit/audit-$(date +%Y-%m-%d).jsonl"
	if [ -f "$AUDIT_FILE" ]; then
		# A http-request comms event for this run produces a JSONL line; assert
		# at least one record exists in today's file (the per-session match is
		# tightened by hand with the session id from the launch banner).
		matches="$(jq -c 'select(.event != null)' "$AUDIT_FILE" 2>/dev/null | wc -l | tr -d ' ')"
		if [ "${matches:-0}" -gt 0 ]; then
			log "ok: R10 today's audit JSONL has $matches event record(s) ($AUDIT_FILE)"
		else
			fail "R10 no event records in $AUDIT_FILE for this run"
			exit 1
		fi
	else
		fail "R10 audit file not found: $AUDIT_FILE"
		exit 1
	fi
else
	log "R10 audit assertion skipped: AILERON_STATE_DIR unset (set it to the daemon state dir to enable)"
fi

log "codex scenario: all assertions passed (R7a, R8.1-8.6, R9, R10)"
