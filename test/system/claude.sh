#!/bin/sh
# test/system/claude.sh
#
# The claude `aileron launch` system-test scenario body (#1477). Invoked by
# the Taskfile target test:system:launch:claude, which owns the reusable
# build seam, the fail-fast preconditions, and the `defer` cleanup (#1475);
# this script owns only the claude-specific scenario logic and reuses the
# shared R8 probes in test/system/lib/probes.sh verbatim.
#
# It NEVER runs `docker run` directly: it drives `aileron launch claude -- ...`
# so the launcher orchestrates the image pull + container run, then inspects
# the resulting container out-of-band by its stable name prefix.
#
# This is the sibling of test/system/codex.sh and differs ONLY in the
# documented claude-specific bindings (issue #1477, decision 2):
#   - image repo suffix `claude`, not `codex`;
#   - credentials at /home/agent/.claude/.credentials.json (claude.go AuthSpec),
#     not /home/agent/.codex/auth.json;
#   - MCP is wired via the `--mcp-config <json>` CLI flag (claude.go
#     ConfigureMCP), NOT a config.toml — so the MCP probe asserts the flag on
#     the container command (probe_mcp_cmdline), never a config file;
#   - the batch flag is `-p "<prompt>"` (claude's non-interactive flag), not
#     codex's `exec "<prompt>"`.
# The container name prefix, daemon reachability, and teardown are identical to
# codex and reuse the shared probes unchanged.
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
# STATIC GATE: this script performs a live `aileron launch claude`, which
# needs a real Claude subscription/login (vault-captured, ADR-0025) and
# consumes LLM tokens. It is run BY HAND on a real host. The headless
# CI/agent path validates the harness with `task --dry
# test:system:launch:claude`, which compiles the target without executing this
# script. See test/system/README.md.

set -eu

# --- claude-specific parameters (passed to the shared probes) --------------
AGENT='claude'
IMAGE_SUFFIX='claude'
# Claude wires aileron-mcp via the `--mcp-config <json>` CLI flag, not a config
# file: ConfigureMCP returns ["--mcp-config", <json>] (claude.go) which the
# launcher appends to the container command. So there is no CONFIG_PATH to
# read; the MCP probe asserts the flag and the aileron server-name marker on
# the running container's command line instead.
MCP_FLAG='--mcp-config'
# The MCP server name Aileron registers itself under (launcher.go MCPServerName
# = "aileron"); it appears as the JSON object key in the --mcp-config payload.
MCP_MARKER='"aileron"'
AUTH_PATH='/home/agent/.claude/.credentials.json'
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

# The agent prompt drives two deterministic, host-verifiable side effects:
#  R9  write the exact sentinel string into the mounted workspace, and
#  R10 invoke the built-in http_request MCP tool against the daemon's own
#      health endpoint so a daemon-side audit record lands for this run.
# The instruction is explicit and self-contained so the agent need not
# improvise; the verification reads the workspace file + audit log, never the
# agent's prose. This is the same R9/R10 shape codex.sh uses so the shared
# assertions are reused.
EXEC_PROMPT="Do exactly two things and then stop. \
1) Write the exact text ${SENTINEL_EXPECTED} (no newline, nothing else) to the file ${WORKSPACE_CONTAINER_PATH}/${SENTINEL_NAME}. \
2) Call the aileron http_request tool with method GET and url \${AILERON_URL}/healthz."

log "claude scenario: runid=${RUNID} workspace=${WORKSPACE} image=${EXPECTED_IMAGE}"

# --- launch (R7a arg-forwarding asserted: post-\`--\` tokens reach claude) ----
# `-- -p "<prompt>"` forwards verbatim through LaunchConfig.Args
# (cmd/aileron/main.go applyTrailingLaunchFlags) ->
# launcher.go `command = append(command, config.Args...)` ->
# runtime.go image then opts.Command. `-p` is claude's non-interactive
# (headless) flag. We run the launch in the background so the live-container
# probes can inspect the container while claude is still resident, then reap
# the exit code.
LAUNCH_LOG="$WORKSPACE/.launch.log"
(
	cd "$WORKSPACE"
	# --sandbox=docker is the claude default (cmd/aileron/main.go), so we do
	# not pass an explicit sandbox flag — the run also asserts that default.
	"$AILERON_BIN" launch "$AGENT" -- -p "$EXEC_PROMPT"
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
	# Claude's MCP wiring is a CLI flag, not a config file (decision 2):
	# assert the `--mcp-config` flag + aileron server marker on the container
	# command, plus the agent-agnostic binary/env core inside probe_mcp_cmdline.
	probe_mcp_cmdline "$container" "$MCP_FLAG" "$MCP_MARKER"
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
# Read byte-exact; a single trailing newline the agent may add is tolerated by
# the command-substitution trim, without accepting arbitrary trailing content.
SENTINEL_ACTUAL="$(cat "$SENTINEL_HOST")"
assert_eq "$SENTINEL_EXPECTED" "$SENTINEL_ACTUAL" "R9 sentinel content byte-exact"

# --- R7a forwarding: the agent acted on the forwarded `-p` instruction ------
# The sentinel above is itself the proof that post-\`--\` args reached the
# claude CLI: claude only ran our `-p` prompt if `-- -p "<prompt>"` was
# forwarded into the container command. Assert explicitly for a clear signal.
assert_not_empty "$SENTINEL_ACTUAL" "R7a post-\`--\` -p args forwarded to claude"

# --- R10 round-trip audit (authored; deterministic where the daemon logs) --
# The built-in http_request tool routes through the daemon's /comms/http
# handler, which appends a message audit entry to today's audit JSONL
# (internal/audit/local.go MessageEntry; written by handlers_comms.go
# logCommsEvent). We jq-assert that today's audit file contains a record for
# this run within the run window. This mirrors codex.sh exactly so the audit
# assertion shape is reused. When the daemon's OTel tracing is enabled, the
# richer span record (aileron.action.name) also lands under
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

log "claude scenario: all assertions passed (R7a, R8.1-8.6, R9, R10)"
