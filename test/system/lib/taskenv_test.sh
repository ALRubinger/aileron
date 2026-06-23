#!/bin/sh
# test/system/lib/taskenv_test.sh
#
# Regression contract for the env wiring of the run-by-hand launch scenarios
# (test:system:launch:codex / :claude). Pure POSIX shell + the `task` binary
# the lib tier already runs under, no Docker and no `aileron launch`, so it is
# CI-safe and unattended.
#
# Why this exists (#1485 regression): the scenario scripts require AILERON_BIN,
# AILERON_SYSTEST_LIB and WORKSPACE in their environment (codex.sh/claude.sh
# fail-fast with `: "${AILERON_BIN:?...}"`). The original wiring declared those
# under an individual `cmds:` entry's `env:` — but go-task ONLY honors `env:`
# at the task level; a command-scoped `env:` is silently dropped. The dry-run
# wiring check (`task --dry`) cannot observe env, so nothing caught it and every
# live run aborted at line 1 of the scenario with "AILERON_BIN must point at the
# built aileron binary". This test pins both halves of the contract:
#   A. behavioral — task-scoped `env:` reaches the invoked script (and a
#      command-scoped `env:` does not), proving the shape the fix depends on.
#   B. structural — the real Taskfile's launch targets keep AILERON_BIN et al.
#      at task scope, so moving them back under a `cmds:` entry fails here
#      rather than silently at the next by-hand run.
#
# Run: sh test/system/lib/taskenv_test.sh   (exit 0 = all cases pass)

set -u

HERE="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# test/system/lib -> repo root
ROOT="$(CDPATH='' cd -- "$HERE/../../.." && pwd)"
TASKFILE="$ROOT/Taskfile.yml"

failures=0
pass() { printf 'PASS: %s\n' "$1"; }
report() {
	# $1 description, $2 ok(0/1)
	if [ "$2" -eq 0 ]; then pass "$1"; else
		printf 'FAIL: %s\n' "$1" >&2
		failures=$((failures + 1))
	fi
}

if ! command -v task >/dev/null 2>&1; then
	# shellcheck disable=SC2016  # the backticks here are literal prose, not a subshell
	printf 'FAIL: `task` binary not on PATH (required to validate env wiring)\n' >&2
	exit 1
fi

WORK="$(mktemp -d 2>/dev/null || mktemp -d -t aileron-taskenv)"
trap 'rm -rf "$WORK"' EXIT INT TERM

# --- A. behavioral: prove go-task's task- vs command-scope env semantics -----
# A probe script the generated tasks invoke; it echoes what it received so we
# can assert from the host side rather than trusting the task echo.
cat >"$WORK/probe.sh" <<'PROBE'
#!/bin/sh
echo "PROBE_BIN=[${AILERON_BIN:-}]"
PROBE

# task-scoped env MUST reach the script.
cat >"$WORK/Taskfile.task.yml" <<EOF
version: '3'
tasks:
  t:
    env:
      AILERON_BIN: 'task-scope-value'
    cmds:
      - cmd: sh "$WORK/probe.sh"
EOF
out_task="$(task -t "$WORK/Taskfile.task.yml" t 2>/dev/null)"
case "$out_task" in
	*"PROBE_BIN=[task-scope-value]"*) report "task-scoped env reaches the scenario script" 0 ;;
	*) report "task-scoped env reaches the scenario script" 1 ;;
esac

# command-scoped env must NOT reach the script — this is the footgun the fix
# routes around; asserting it documents why the env lives at task scope.
cat >"$WORK/Taskfile.cmd.yml" <<EOF
version: '3'
tasks:
  t:
    cmds:
      - cmd: sh "$WORK/probe.sh"
        env:
          AILERON_BIN: 'cmd-scope-value'
EOF
out_cmd="$(task -t "$WORK/Taskfile.cmd.yml" t 2>/dev/null)"
case "$out_cmd" in
	*"PROBE_BIN=[cmd-scope-value]"*) report "command-scoped env is dropped by go-task (regression footgun)" 1 ;;
	*) report "command-scoped env is dropped by go-task (regression footgun)" 0 ;;
esac

# --- B. structural: the real launch targets keep env at task scope -----------
# For each target block, every required key must appear at the 6-space indent
# of a task-level `env:` child, and never at the 10-space indent of an `env:`
# nested under a `- cmd:` list item. awk extracts the block from its header to
# the next top-level (2-space) task key.
required_keys='AILERON_BIN AILERON_SYSTEST_LIB WORKSPACE'

block_for() {
	# $1 = task name (e.g. test:system:launch:codex)
	awk -v target="  $1:" '
		$0 == target { inblk = 1; next }
		inblk && /^  [^ ]/ { inblk = 0 }
		inblk { print }
	' "$TASKFILE"
}

env_section_for() {
	# $1 = task name. Isolates the lines under the task-level `env:` key (its
	# 6-space children) so the positive check can't be satisfied by a key of
	# the same name elsewhere in the block (e.g. WORKSPACE also lives under
	# `vars:`). Stops at the next 4-space task key (cmds:/deps:/vars:/env:).
	block_for "$1" | awk '
		/^    env:[[:space:]]*$/ { inenv = 1; next }
		inenv && /^    [^ ]/ { inenv = 0 }
		inenv { print }
	'
}

check_target() {
	# $1 = task name
	blk="$(block_for "$1")"
	if [ -z "$blk" ]; then
		report "$1: target block found in Taskfile.yml" 1
		return
	fi
	env_blk="$(env_section_for "$1")"
	for key in $required_keys; do
		# Good: the key is a child of the task-level `env:` block (6-space indent).
		if printf '%s\n' "$env_blk" | grep -q "^      ${key}:"; then
			report "$1: ${key} declared under task-level env" 0
		else
			report "$1: ${key} declared under task-level env" 1
		fi
		# Bad: env nested under a `- cmd:` entry puts the key at 10 spaces.
		if printf '%s\n' "$blk" | grep -q "^          ${key}:"; then
			report "$1: ${key} NOT nested under a cmds entry (go-task would drop it)" 1
		else
			report "$1: ${key} NOT nested under a cmds entry (go-task would drop it)" 0
		fi
	done
}

check_target test:system:launch:codex
check_target test:system:launch:claude

if [ "$failures" -ne 0 ]; then
	printf '\n%s task-env wiring case(s) FAILED\n' "$failures" >&2
	exit 1
fi
printf '\nall task-env wiring contract cases passed\n'
