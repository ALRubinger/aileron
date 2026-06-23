# shellcheck shell=sh
# test/system/lib/assert.sh
#
# Generic assertion + logging helpers for the `aileron launch` system-test
# scenarios (#1476 codex, #1477 claude). Pure POSIX shell so it parses and
# runs under Task's embedded mvdan/sh interpreter on every host (macOS,
# Linux, Windows/Git-Bash) without a system bash.
#
# Every helper writes an actionable message to stderr and returns non-zero
# on violation; nothing here calls `exit`, so a caller can decide whether a
# failed assertion aborts the scenario or is merely recorded. The scenarios
# run with `set -e` (the default under Task), so a non-zero return aborts the
# command block and Task's `defer:` cleanup still fires.
#
# Sourced, never executed. Source it once near the top of a scenario:
#   . "$AILERON_SYSTEST_LIB/assert.sh"

# log emits an informational line to stderr, prefixed so scenario output is
# greppable and never mistaken for an assertion failure.
log() {
	printf 'systest: %s\n' "$*" >&2
}

# fail emits an error line to stderr and returns 1. Use it for bespoke
# checks that the assert_* helpers below do not cover.
fail() {
	printf 'systest: FAIL: %s\n' "$*" >&2
	return 1
}

# assert_eq <expected> <actual> <what>
# Byte-exact string equality. On mismatch prints both values so the failure
# is diagnosable from CI logs alone.
assert_eq() {
	# $1 expected, $2 actual, $3 description
	if [ "$1" = "$2" ]; then
		log "ok: $3"
		return 0
	fi
	printf 'systest: FAIL: %s\n  expected: %s\n  actual:   %s\n' "$3" "$1" "$2" >&2
	return 1
}

# assert_contains <haystack> <needle> <what>
# Substring containment. Uses a case glob rather than grep so it works on a
# value that contains newlines and needs no subprocess.
assert_contains() {
	# $1 haystack, $2 needle, $3 description
	case "$1" in
	*"$2"*)
		log "ok: $3"
		return 0
		;;
	esac
	printf 'systest: FAIL: %s\n  expected to contain: %s\n  in: %s\n' "$3" "$2" "$1" >&2
	return 1
}

# assert_not_empty <value> <what>
assert_not_empty() {
	# $1 value, $2 description
	if [ -n "$1" ]; then
		log "ok: $2"
		return 0
	fi
	fail "$2 (value was empty)"
}
