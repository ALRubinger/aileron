#!/bin/sh
# test/system/lib/assert_test.sh
#
# Contract tests for the shared assert.sh helpers. Pure POSIX shell, no
# Docker, no `aileron launch` — so this runs in CI and unattended. Each case
# states the expected behaviour from the helper's contract (return 0 on the
# satisfied condition, non-zero with a stderr message on violation) and
# exercises both the happy path and the documented failure path.
#
# Run: sh test/system/lib/assert_test.sh   (exit 0 = all cases pass)

set -u

HERE="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# shellcheck source=test/system/lib/assert.sh
. "$HERE/assert.sh"

failures=0
check() {
	# $1 description, $2 actual-rc, $3 expected-rc
	if [ "$2" -eq "$3" ]; then
		printf 'PASS: %s\n' "$1"
	else
		printf 'FAIL: %s (rc=%s, want %s)\n' "$1" "$2" "$3" >&2
		failures=$((failures + 1))
	fi
}

# assert_eq: equal -> 0, unequal -> 1
assert_eq "a" "a" "eq-happy" >/dev/null 2>&1
check "assert_eq returns 0 on equal" "$?" 0
assert_eq "a" "b" "eq-fail" >/dev/null 2>&1
check "assert_eq returns non-zero on unequal" "$?" 1

# assert_contains: substring present -> 0, absent -> 1
assert_contains "hello world" "lo wo" "contains-happy" >/dev/null 2>&1
check "assert_contains returns 0 when substring present" "$?" 0
assert_contains "hello world" "xyz" "contains-fail" >/dev/null 2>&1
check "assert_contains returns non-zero when substring absent" "$?" 1
# Robust to newlines in the haystack (env dumps, mount lists).
assert_contains "$(printf 'a\nbind:/home/agent/.codex\nc')" "bind:/home/agent/.codex" "contains-newline" >/dev/null 2>&1
check "assert_contains handles a multi-line haystack" "$?" 0

# assert_not_empty: non-empty -> 0, empty -> 1
assert_not_empty "x" "notempty-happy" >/dev/null 2>&1
check "assert_not_empty returns 0 on non-empty" "$?" 0
assert_not_empty "" "notempty-fail" >/dev/null 2>&1
check "assert_not_empty returns non-zero on empty" "$?" 1

# fail always returns 1 and writes to stderr.
msg="$(fail "boom" 2>&1)"; rc=$?
check "fail returns non-zero" "$rc" 1
assert_contains "$msg" "boom" "fail-message" >/dev/null 2>&1
check "fail writes its message to stderr" "$?" 0

if [ "$failures" -ne 0 ]; then
	printf '\n%s assertion-helper case(s) FAILED\n' "$failures" >&2
	exit 1
fi
printf '\nall assert.sh contract cases passed\n'
