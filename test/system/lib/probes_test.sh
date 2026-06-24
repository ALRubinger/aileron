#!/bin/sh
# test/system/lib/probes_test.sh
#
# Contract tests for the shared probes.sh helpers that have host-verifiable
# logic independent of a live container: discover_container's match-count
# arbitration (zero / one / many) and the pure probe_exit_code assertion.
# `docker` is stubbed on PATH so the tests run in CI with no Docker daemon
# and no `aileron launch`.
#
# The container-introspection probes (probe_image / probe_mcp /
# probe_credentials / probe_daemon_reachable) require a real running
# container and are exercised by the by-hand scenario run documented in
# test/system/README.md, not here.
#
# Run: sh test/system/lib/probes_test.sh   (exit 0 = all cases pass)

set -u

HERE="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# shellcheck source=test/system/lib/assert.sh
. "$HERE/assert.sh"
# shellcheck source=test/system/lib/probes.sh
. "$HERE/probes.sh"

failures=0
check() {
	if [ "$2" -eq "$3" ]; then
		printf 'PASS: %s\n' "$1"
	else
		printf 'FAIL: %s (rc=%s, want %s)\n' "$1" "$2" "$3" >&2
		failures=$((failures + 1))
	fi
}

# Stub `docker` so the host-verifiable probes can run with no daemon:
#   docker ps ...        -> DOCKER_PS_OUTPUT (discover_container)
#   docker exec ... test  -> DOCKER_EXEC_TEST_RC (probe_mcp_runtime: -x checks)
#   docker exec ... printenv -> DOCKER_EXEC_PRINTENV_RC (env-var checks)
#   docker inspect -f ... -> DOCKER_INSPECT_OUTPUT (probe_mcp_cmdline cmdline)
# The stub dir goes on the front of PATH for the duration of the test.
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR"' EXIT INT TERM
cat >"$STUB_DIR/docker" <<'STUB'
#!/bin/sh
# Minimal docker stub for the host-verifiable probe contract tests.
case "$1" in
ps) printf '%s' "${DOCKER_PS_OUTPUT:-}" ;;
inspect) printf '%s' "${DOCKER_INSPECT_OUTPUT:-}" ;;
exec)
	# Args after `exec` are: <container> <cmd> [args...]. Route on <cmd>.
	shift
	container="$1"
	cmd="$2"
	case "$cmd" in
	test) exit "${DOCKER_EXEC_TEST_RC:-0}" ;;
	printenv) exit "${DOCKER_EXEC_PRINTENV_RC:-0}" ;;
	*) exit 0 ;;
	esac
	;;
*) exit 0 ;;
esac
STUB
chmod +x "$STUB_DIR/docker"
PATH="$STUB_DIR:$PATH"
export PATH

# discover_container: exactly one match -> echoes the name, returns 0.
out="$(DOCKER_PS_OUTPUT='aileron-sbx-codex-123
' discover_container 'aileron-sbx-' 2>/dev/null)"; rc=$?
check "discover_container returns 0 on a single match" "$rc" 0
assert_eq "aileron-sbx-codex-123" "$out" "discover_container echoes the matched name" >/dev/null 2>&1
check "discover_container echoes the single matched name" "$?" 0

# discover_container: zero matches -> non-zero (never silently empty).
DOCKER_PS_OUTPUT='' discover_container 'aileron-sbx-' >/dev/null 2>&1
check "discover_container returns non-zero on zero matches" "$?" 1

# discover_container: more than one match -> non-zero (refuses to guess).
DOCKER_PS_OUTPUT='aileron-sbx-a
aileron-sbx-b
' discover_container 'aileron-sbx-' >/dev/null 2>&1
check "discover_container returns non-zero on multiple matches" "$?" 1

# probe_exit_code: matching code -> 0, mismatch -> non-zero.
probe_exit_code 0 0 >/dev/null 2>&1
check "probe_exit_code returns 0 when codes match" "$?" 0
probe_exit_code 1 0 >/dev/null 2>&1
check "probe_exit_code returns non-zero when codes differ" "$?" 1

# --- image_repo / image_ref_matches: R8.1 digest-tolerant image equality -----
# These are pure string helpers (no docker), so they are host-verifiable here.

# image_repo strips a floating tag, a digest, or a bare ref to the same repo.
out="$(image_repo 'ghcr.io/alrubinger/aileron-sandbox-codex:edge')"
assert_eq "ghcr.io/alrubinger/aileron-sandbox-codex" "$out" "image_repo strips :edge" >/dev/null 2>&1
check "image_repo strips a floating tag" "$?" 0
out="$(image_repo 'ghcr.io/alrubinger/aileron-sandbox-codex@sha256:abc123')"
assert_eq "ghcr.io/alrubinger/aileron-sandbox-codex" "$out" "image_repo strips @sha256 digest" >/dev/null 2>&1
check "image_repo strips an @sha256 digest" "$?" 0
out="$(image_repo 'localhost:5000/aileron-sandbox-codex')"
assert_eq "localhost:5000/aileron-sandbox-codex" "$out" "image_repo keeps a registry host:port with no tag" >/dev/null 2>&1
check "image_repo keeps a registry host:port when there is no tag" "$?" 0

# A floating-tag expected ref matches the same floating tag.
image_ref_matches 'ghcr.io/alrubinger/aileron-sandbox-codex:edge' \
	'ghcr.io/alrubinger/aileron-sandbox-codex:edge' >/dev/null 2>&1
check "image_ref_matches accepts the same floating tag" "$?" 0

# THE REGRESSION: a floating expected ref matches the digest the launcher
# actually pulled on a reproducible release (#1233). Strict assert_eq failed
# here; image_ref_matches must accept it.
image_ref_matches 'ghcr.io/alrubinger/aileron-sandbox-codex:edge' \
	'ghcr.io/alrubinger/aileron-sandbox-codex@sha256:deadbeef' >/dev/null 2>&1
check "image_ref_matches accepts a digest pin on the same repo (release path)" "$?" 0

# A :latest expected ref also matches a digest on the same repo.
image_ref_matches 'ghcr.io/alrubinger/aileron-sandbox-codex:latest' \
	'ghcr.io/alrubinger/aileron-sandbox-codex@sha256:deadbeef' >/dev/null 2>&1
check "image_ref_matches accepts a digest pin against a :latest expectation" "$?" 0

# A DIFFERENT repo/agent (claude image) must still fail even via a digest.
image_ref_matches 'ghcr.io/alrubinger/aileron-sandbox-codex:edge' \
	'ghcr.io/alrubinger/aileron-sandbox-claude@sha256:deadbeef' >/dev/null 2>&1
check "image_ref_matches rejects a digest pin on a different agent repo" "$?" 1

# A wrong (non-floating, non-digest) tag on the right repo must still fail.
image_ref_matches 'ghcr.io/alrubinger/aileron-sandbox-codex:edge' \
	'ghcr.io/alrubinger/aileron-sandbox-codex:v0.0.1' >/dev/null 2>&1
check "image_ref_matches rejects an unexpected tag on the right repo" "$?" 1

# An explicit EXPECTED_IMAGE digest override matches exactly.
image_ref_matches 'ghcr.io/alrubinger/aileron-sandbox-codex@sha256:abc' \
	'ghcr.io/alrubinger/aileron-sandbox-codex@sha256:abc' >/dev/null 2>&1
check "image_ref_matches accepts an exact digest override" "$?" 0

# --- LOCAL (no registry host) tag: the recipe'd-but-not-publishable agent -----
# claude (#1451/#1585) resolves LocalAgentImageTag = `aileron/sandbox-agent-
# claude:<tag>` (composition.go:188-198, scaffold.go:106-108), a local-daemon
# repo with NO registry host. The shared probe must derive the repo from the
# actual ref and accept :edge/:latest on it, exactly as it does for the GHCR
# repo — that is what lets claude reuse probes.sh verbatim. This locks that
# contract so the R8.1 probe handles a host-less local tag.

# image_repo strips :edge from a no-registry-host local ref.
out="$(image_repo 'aileron/sandbox-agent-claude:edge')"
assert_eq "aileron/sandbox-agent-claude" "$out" "image_repo strips :edge from a local ref" >/dev/null 2>&1
check "image_repo strips the tag from a no-registry-host ref" "$?" 0

# image_repo strips :latest from the same local ref (release-path tag).
out="$(image_repo 'aileron/sandbox-agent-claude:latest')"
assert_eq "aileron/sandbox-agent-claude" "$out" "image_repo strips :latest from a local ref" >/dev/null 2>&1
check "image_repo strips :latest from a no-registry-host ref" "$?" 0

# image_ref_matches accepts the local :edge tag on its own repo (claude default).
image_ref_matches 'aileron/sandbox-agent-claude:edge' \
	'aileron/sandbox-agent-claude:edge' >/dev/null 2>&1
check "image_ref_matches accepts the local :edge tag on its own repo" "$?" 0

# image_ref_matches accepts the local :latest tag on its own repo (release path).
# claude has no GHCR-published digest, so :latest is the release shape here.
image_ref_matches 'aileron/sandbox-agent-claude:latest' \
	'aileron/sandbox-agent-claude:latest' >/dev/null 2>&1
check "image_ref_matches accepts the local :latest tag on its own repo" "$?" 0

# A wrong repo (codex) must still fail against the local claude expectation.
image_ref_matches 'aileron/sandbox-agent-claude:edge' \
	'aileron/sandbox-agent-codex:edge' >/dev/null 2>&1
check "image_ref_matches rejects a different local agent repo" "$?" 1

# A wrong (non-floating) tag on the right local repo must still fail.
image_ref_matches 'aileron/sandbox-agent-claude:edge' \
	'aileron/sandbox-agent-claude:v0.0.1' >/dev/null 2>&1
check "image_ref_matches rejects an unexpected tag on the local repo" "$?" 1

# --- probe_mcp_runtime: the agent-agnostic R8.2 core (binary + env) ----------
# All sub-checks green: aileron-mcp present (test -x rc 0) and every daemon env
# var set (printenv rc 0) -> returns 0.
DOCKER_EXEC_TEST_RC=0 DOCKER_EXEC_PRINTENV_RC=0 \
	probe_mcp_runtime 'c' >/dev/null 2>&1
check "probe_mcp_runtime returns 0 when binary present and env set" "$?" 0

# aileron-mcp missing/not executable (test -x rc 1) -> non-zero, before env.
DOCKER_EXEC_TEST_RC=1 DOCKER_EXEC_PRINTENV_RC=0 \
	probe_mcp_runtime 'c' >/dev/null 2>&1
check "probe_mcp_runtime returns non-zero when aileron-mcp missing" "$?" 1

# Binary present but a daemon-wiring env var unset (printenv rc 1) -> non-zero.
DOCKER_EXEC_TEST_RC=0 DOCKER_EXEC_PRINTENV_RC=1 \
	probe_mcp_runtime 'c' >/dev/null 2>&1
check "probe_mcp_runtime returns non-zero when a daemon env var is unset" "$?" 1

# --- probe_mcp_cmdline: claude's --mcp-config flag on the container command ---
# The agent-agnostic core passes (binary + env), and the inspected command line
# carries both the flag and the aileron server marker -> returns 0.
DOCKER_EXEC_TEST_RC=0 DOCKER_EXEC_PRINTENV_RC=0 \
	DOCKER_INSPECT_OUTPUT='claude --mcp-config {"mcpServers":{"aileron":{}}} -p hi ' \
	probe_mcp_cmdline 'c' '--mcp-config' '"aileron"' >/dev/null 2>&1
check "probe_mcp_cmdline returns 0 when flag and marker both present" "$?" 0

# Flag present but the payload does NOT reference the aileron server marker
# (e.g. a stray --mcp-config with the wrong server) -> non-zero.
DOCKER_EXEC_TEST_RC=0 DOCKER_EXEC_PRINTENV_RC=0 \
	DOCKER_INSPECT_OUTPUT='claude --mcp-config {"mcpServers":{"other":{}}} -p hi ' \
	probe_mcp_cmdline 'c' '--mcp-config' '"aileron"' >/dev/null 2>&1
check "probe_mcp_cmdline returns non-zero when marker absent from payload" "$?" 1

# No --mcp-config flag on the command at all -> non-zero.
DOCKER_EXEC_TEST_RC=0 DOCKER_EXEC_PRINTENV_RC=0 \
	DOCKER_INSPECT_OUTPUT='claude -p hi ' \
	probe_mcp_cmdline 'c' '--mcp-config' '"aileron"' >/dev/null 2>&1
check "probe_mcp_cmdline returns non-zero when --mcp-config flag absent" "$?" 1

# Command line is right but the agent-agnostic core fails first (binary
# missing) -> non-zero, proving the core gates before the cmdline check.
DOCKER_EXEC_TEST_RC=1 DOCKER_EXEC_PRINTENV_RC=0 \
	DOCKER_INSPECT_OUTPUT='claude --mcp-config {"mcpServers":{"aileron":{}}} -p hi ' \
	probe_mcp_cmdline 'c' '--mcp-config' '"aileron"' >/dev/null 2>&1
check "probe_mcp_cmdline returns non-zero when the runtime core fails" "$?" 1

if [ "$failures" -ne 0 ]; then
	printf '\n%s probe contract case(s) FAILED\n' "$failures" >&2
	exit 1
fi
printf '\nall probes.sh contract cases passed\n'
