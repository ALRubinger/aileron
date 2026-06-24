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

# --- C. structural: SESSION/WORKSPACE vars are Unix-shell-free (#1547) --------
# The three run-by-hand system-test targets must not derive SESSION/WORKSPACE via
# `date +%s`, `$$`, `mktemp`, or a hardcoded `/tmp` LHS — go-task's embedded shell
# has no `date` and a `mktemp` that can't resolve /tmp on Windows, so those forms
# fail before any test logic. Portable forms use pure template funcs
# (`now | unixEpoch`, `randInt`) and env-var-derived temp roots.
vars_section_for() {
	# $1 = task name. Isolates the lines under the task-level `vars:` key (its
	# 6-space children), stopping at the next 4-space task key
	# (cmds:/deps:/vars:/env:).
	block_for "$1" | awk '
		/^    vars:[[:space:]]*$/ { invars = 1; next }
		invars && /^    [^ ]/ { invars = 0 }
		invars { print }
	'
}

# The default-chain temp root (`/tmp` only as the final `default` fallback) is
# the one sanctioned literal `/tmp`; flag any OTHER `/tmp` (e.g. a hardcoded
# `/tmp/aileron-...` LHS). Strip the sanctioned token before scanning.
var_subblock_for() {
	# $1 = task name, $2 = var key. Prints the var's definition sub-block: the
	# `KEY:` line (6-space indent) plus any deeper-indented children (e.g. a
	# legacy `sh:` line at 8-space indent), stopping at the next 6-space var key
	# or any comment line. Catches both the inline form (`KEY: '...'`) and the
	# old multi-line dynamic form (`KEY:` / `  sh: ...`).
	vars_section_for "$1" | awk -v key="      $2:" '
		index($0, key) == 1 { invar = 1; print; next }
		invar && /^      [^ ]/ { invar = 0 }
		invar && /^      #/ { invar = 0 }
		invar { print }
	'
}

forbidden_in_vars() {
	# $1 = task name. Prints any offending var definition line, empty if clean.
	# Scans the SESSION/WORKSPACE definition sub-blocks (inline value and any
	# `sh:` children), never the explanatory comments around them. The sanctioned
	# `default "/tmp"` fallback is stripped before scanning.
	for k in SESSION WORKSPACE; do
		var_subblock_for "$1" "$k"
	done \
		| sed 's#default "/tmp"##g' \
		| grep -nE 'date \+%s|\$\$|mktemp|/tmp' || true
}

for t in test:system:smoke test:system:launch:codex test:system:launch:claude; do
	offending="$(forbidden_in_vars "$t")"
	if [ -z "$offending" ]; then
		report "$t: SESSION/WORKSPACE vars use no date/\$\$/mktemp//tmp (portable)" 0
	else
		printf 'offending var line(s) in %s:\n%s\n' "$t" "$offending" >&2
		report "$t: SESSION/WORKSPACE vars use no date/\$\$/mktemp//tmp (portable)" 1
	fi
done

# --- D. behavioral: vars resolve with `date`+`mktemp` off PATH (#1547) --------
# Simulates go-task's Windows embedded-shell condition (no `date`, no usable
# `mktemp`) by extracting each target's real SESSION/WORKSPACE var lines into a
# minimal Taskfile and resolving them under a PATH that contains the `task` and
# `sh` binaries but NOT `date`/`mktemp`. Pure-template vars resolve regardless;
# the old `sh:` dynamic vars would fail here (this case fails before the fix).
TASK_BIN="$(command -v task)"
SH_BIN="$(command -v sh)"
SANDBOX_PATH="$WORK/nobins"
mkdir -p "$SANDBOX_PATH"
ln -s "$TASK_BIN" "$SANDBOX_PATH/task"
ln -s "$SH_BIN" "$SANDBOX_PATH/sh"
# Confirm the simulation is real: `date`/`mktemp` must be absent under this PATH.
if PATH="$SANDBOX_PATH" command -v date >/dev/null 2>&1 ||
	PATH="$SANDBOX_PATH" command -v mktemp >/dev/null 2>&1; then
	report "sandbox PATH excludes date/mktemp (Windows embedded-shell sim)" 1
else
	report "sandbox PATH excludes date/mktemp (Windows embedded-shell sim)" 0
fi

resolve_var_under_sandbox() {
	# $1 = task name, $2 = var key (SESSION|WORKSPACE). Echoes the resolved value.
	# Pull the exact `KEY: '...'` line from the real Taskfile's vars block so the
	# behavioral case tracks whatever the structural case pinned.
	varline="$(vars_section_for "$1" | grep -E "^      $2:" | head -n1 | sed 's/^      //')"
	[ -n "$varline" ] || { echo ""; return; }
	gen="$WORK/Taskfile.$2.$(echo "$1" | tr ':' '_').yml"
	{
		echo "version: '3'"
		echo "tasks:"
		echo "  r:"
		echo "    vars:"
		echo "      $varline"
		echo "    cmds:"
		echo "      - 'echo RESOLVED=[{{.$2}}]'"
	} >"$gen"
	out="$(PATH="$SANDBOX_PATH" "$TASK_BIN" -t "$gen" r 2>/dev/null)"
	# Extract the value inside RESOLVED=[...].
	printf '%s\n' "$out" | sed -n 's/.*RESOLVED=\[\(.*\)\].*/\1/p' | head -n1
}

for t in test:system:smoke test:system:launch:codex test:system:launch:claude; do
	for key in SESSION WORKSPACE; do
		val="$(resolve_var_under_sandbox "$t" "$key")"
		if [ -n "$val" ]; then
			report "$t: $key resolves non-empty with date/mktemp off PATH" 0
		else
			report "$t: $key resolves non-empty with date/mktemp off PATH" 1
		fi
	done
done

# --- E. host-arch build outputs carry the host exe suffix (#1590) ------------
# The local host-arch build tasks pass an explicit `-o build/<name>`, so the Go
# toolchain writes that exact name and skips the platform `.exe` append it only
# performs when `-o` is absent. On Windows that left build/aileron(.exe-less)
# while mcp:setup registered ./build/aileron-mcp and the launch scenarios pointed
# AILERON_BIN at build/aileron, none of which matched the file the toolchain
# actually wrote (build/aileron.exe). The fix introduces a HOST_EXE var sourced
# from the canonical `go env GOEXE` (empty on Unix/macOS, ".exe" on Windows) and
# appends {{.HOST_EXE}} to the three host-arch outputs and every path that
# consumes them. This section pins both halves:
#   E1. structural — HOST_EXE is sourced from `go env GOEXE`; it is scoped
#       per-task (NOT a top-level global var, whose eager `sh:` eval would break
#       Go-free targets like build:docs/webapp); every task that references
#       {{.HOST_EXE}} also defines it; the host-arch outputs and their consumers
#       append it; and the forced GOOS=linux sandbox sibling stays extensionless
#       (it is bind-mounted into a Linux container, where `.exe` would be wrong).
#   E2. behavioral — with GOEXE=.exe (Windows), AILERON_BIN and the host-arch
#       build `-o` flags resolve WITH the .exe suffix while the Linux sibling
#       stays bare; with GOEXE empty (Unix), the outputs are unchanged.

# E1a. Every HOST_EXE definition is sourced from `go env GOEXE`. Each `HOST_EXE:`
# key in the file must be immediately followed by an `sh: go env GOEXE` child;
# a literal or any other source would drift from the canonical host primitive.
hostexe_defs="$(grep -c '^      HOST_EXE:' "$TASKFILE")"
hostexe_goexe="$(grep -A1 '^      HOST_EXE:' "$TASKFILE" | grep -c 'sh: go env GOEXE')"
if [ "$hostexe_defs" -ge 1 ] && [ "$hostexe_defs" -eq "$hostexe_goexe" ]; then
	report "every HOST_EXE definition is sourced from \`go env GOEXE\` ($hostexe_defs found)" 0
else
	printf 'HOST_EXE defs=%s, go-env-GOEXE-sourced=%s (must be equal and >=1)\n' "$hostexe_defs" "$hostexe_goexe" >&2
	report "every HOST_EXE definition is sourced from \`go env GOEXE\`" 1
fi

# E1a2. HOST_EXE is NOT a top-level (global) var. A global `sh:` var is evaluated
# eagerly by go-task on EVERY task invocation, so a global `go env GOEXE` would
# abort Go-free targets (build:docs, build:webapp, up) on a host without `go`.
# The fix scopes it per-task instead; a 2-space-indented `HOST_EXE:` under the
# top-level `vars:` block is the regression this guards against.
global_hostexe="$(awk '
	/^vars:[[:space:]]*$/ { invars = 1; next }
	invars && /^[^ ]/ { invars = 0 }
	invars && /^  HOST_EXE:/ { print }
' "$TASKFILE")"
if [ -z "$global_hostexe" ]; then
	report "HOST_EXE is NOT a top-level global var (no eager go-env on Go-free tasks)" 0
else
	report "HOST_EXE is NOT a top-level global var (no eager go-env on Go-free tasks)" 1
fi

# E1a3. Every task that REFERENCES {{.HOST_EXE}} also DEFINES it in its own vars.
# A `{{.HOST_EXE}}` reference in a task that lacks a `HOST_EXE:` var resolves to
# the empty string under go-task (no error), silently reintroducing the bug on
# Windows. Walk each task block: if it mentions {{.HOST_EXE}}, it must also
# declare `HOST_EXE:` at the 6-space var indent.
ref_tasks="$(awk '
	# A task header is a 2-space-indented name ending in `:` (names may contain
	# internal colons, e.g. test:system:smoke), so strip only the trailing `:`.
	/^  [a-zA-Z][a-zA-Z0-9:_-]*:[[:space:]]*$/ { name = $0; sub(/^  /, "", name); sub(/:[[:space:]]*$/, "", name) }
	/\{\{\.HOST_EXE\}\}/ { if (name != "") print name }
' "$TASKFILE" | sort -u)"
for t in $ref_tasks; do
	if block_for "$t" | grep -q '^      HOST_EXE:'; then
		report "$t: references {{.HOST_EXE}} and defines it in task vars" 0
	else
		report "$t: references {{.HOST_EXE}} and defines it in task vars" 1
	fi
done

# E1b. The three host-arch build outputs append {{.HOST_EXE}}. Each `-o` flag for
# a host binary (server/mcp/cli) must be immediately followed by {{.HOST_EXE}};
# a bare `-o build/aileron ` (no suffix) is the pre-fix regression.
for spec in 'aileron-server' 'aileron-mcp' 'aileron'; do
	# Match the host-arch line: `-o build/<name>{{.HOST_EXE}} `. The mcp name is a
	# prefix of the linux sibling and `aileron` a prefix of both others, so anchor
	# on the trailing `{{.HOST_EXE}} ` to avoid cross-matching.
	if grep -qE "\-o build/${spec}\{\{\.HOST_EXE\}\} " "$TASKFILE"; then
		report "build output build/${spec} appends {{.HOST_EXE}}" 0
	else
		report "build output build/${spec} appends {{.HOST_EXE}}" 1
	fi
	# Guard the regression directly: no bare `-o build/<name> ` (suffix-less)
	# survives for a host target.
	if grep -qE "\-o build/${spec} " "$TASKFILE"; then
		report "no suffix-less -o build/${spec} remains (Windows regression)" 1
	else
		report "no suffix-less -o build/${spec} remains (Windows regression)" 0
	fi
done

# E1c. The forced GOOS=linux sandbox sibling MUST stay extensionless — it is
# bind-mounted into a Linux container.
if grep -qE "\-o build/aileron-mcp-linux-\{\{\.HOST_GOARCH\}\} " "$TASKFILE"; then
	report "linux sandbox sibling stays extensionless (no HOST_EXE)" 0
else
	report "linux sandbox sibling stays extensionless (no HOST_EXE)" 1
fi

# E1d. The consumers (mcp:setup registration, both launch scenarios' AILERON_BIN)
# append {{.HOST_EXE}} so they match the built file on Windows.
if grep -qE "mcp add .* \./build/aileron-mcp\{\{\.HOST_EXE\}\}" "$TASKFILE"; then
	report "mcp:setup registers ./build/aileron-mcp{{.HOST_EXE}}" 0
else
	report "mcp:setup registers ./build/aileron-mcp{{.HOST_EXE}}" 1
fi
ailbin_count="$(grep -cE "AILERON_BIN: '\{\{\.ROOT_DIR\}\}/build/aileron\{\{\.HOST_EXE\}\}'" "$TASKFILE")"
if [ "$ailbin_count" -eq 2 ]; then
	report "both launch scenarios point AILERON_BIN at build/aileron{{.HOST_EXE}}" 0
else
	printf 'expected 2 AILERON_BIN+HOST_EXE refs, found %s\n' "$ailbin_count" >&2
	report "both launch scenarios point AILERON_BIN at build/aileron{{.HOST_EXE}}" 1
fi

# E2. Behavioral: resolve a HOST_EXE-bearing template under a forced GOEXE and
# confirm the suffix flows through. A tiny Taskfile mirrors the real var shape
# (HOST_EXE from `go env GOEXE`) and a path that consumes it; we drive `go env
# GOEXE` deterministically via GOOS so this is hermetic regardless of host OS.
resolve_hostexe_path() {
	# $1 = GOOS to force (windows -> .exe, the native value -> usually empty).
	gen="$WORK/Taskfile.hostexe.yml"
	{
		echo "version: '3'"
		echo "vars:"
		echo "  HOST_EXE:"
		echo "    sh: go env GOEXE"
		echo "tasks:"
		echo "  r:"
		echo "    cmds:"
		echo "      - 'echo RESOLVED=[build/aileron{{.HOST_EXE}}]'"
	} >"$gen"
	out="$(GOOS="$1" "$TASK_BIN" -t "$gen" r 2>/dev/null)"
	printf '%s\n' "$out" | sed -n 's/.*RESOLVED=\[\(.*\)\].*/\1/p' | head -n1
}

if command -v go >/dev/null 2>&1; then
	win_path="$(resolve_hostexe_path windows)"
	if [ "$win_path" = "build/aileron.exe" ]; then
		report "HOST_EXE resolves to .exe under GOOS=windows (path = build/aileron.exe)" 0
	else
		printf 'got Windows path [%s], expected [build/aileron.exe]\n' "$win_path" >&2
		report "HOST_EXE resolves to .exe under GOOS=windows (path = build/aileron.exe)" 1
	fi
	# Native resolution: whatever this host's GOEXE is, the path must be a valid
	# build/aileron(.exe)? form and, on a non-Windows host, suffix-less.
	native_goexe="$(go env GOEXE)"
	native_path="$(resolve_hostexe_path "$(go env GOOS)")"
	if [ "$native_path" = "build/aileron${native_goexe}" ]; then
		report "HOST_EXE resolves to host GOEXE natively (path = build/aileron${native_goexe})" 0
	else
		printf 'got native path [%s], expected [build/aileron%s]\n' "$native_path" "$native_goexe" >&2
		report "HOST_EXE resolves to host GOEXE natively (path = build/aileron${native_goexe})" 1
	fi
else
	# `go` is a hard prerequisite for these builds; its absence is a real gap.
	report "go binary on PATH (required to resolve HOST_EXE behaviorally)" 1
fi

if [ "$failures" -ne 0 ]; then
	printf '\n%s task-env wiring case(s) FAILED\n' "$failures" >&2
	exit 1
fi
printf '\nall task-env wiring contract cases passed\n'
