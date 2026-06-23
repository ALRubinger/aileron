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

# Stub `docker` so `docker ps ...` emits whatever DOCKER_PS_OUTPUT holds.
# The stub dir goes on the front of PATH for the duration of the test.
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR"' EXIT INT TERM
cat >"$STUB_DIR/docker" <<'STUB'
#!/bin/sh
# Minimal docker stub: only `docker ps ...` is used by discover_container.
case "$1" in
ps) printf '%s' "${DOCKER_PS_OUTPUT:-}" ;;
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

if [ "$failures" -ne 0 ]; then
	printf '\n%s probe contract case(s) FAILED\n' "$failures" >&2
	exit 1
fi
printf '\nall probes.sh contract cases passed\n'
