# shellcheck shell=sh
# test/system/lib/probes.sh
#
# Agent-agnostic R8 wiring-invariant probes shared by the `aileron launch`
# system-test scenarios (#1476 codex, #1477 claude). Pure POSIX shell so it
# parses and runs under Task's embedded mvdan/sh interpreter on every host.
#
# Each probe inspects a *running* sandbox container out-of-band with
# `docker inspect` / `docker exec` and returns non-zero with an actionable
# message on violation. The scenario drives `aileron launch <agent> -- ...`
# (never `docker run` directly) so the launcher orchestrates the image pull
# and container run; these probes only observe the result.
#
# Codex-specific values (image suffix `codex`, config
# /home/agent/.codex/config.toml, auth /home/agent/.codex/auth.json) are
# passed in as arguments, never hard-coded here, so the claude scenario
# reuses every function verbatim.
#
# Sourced after assert.sh:
#   . "$AILERON_SYSTEST_LIB/assert.sh"
#   . "$AILERON_SYSTEST_LIB/probes.sh"

# The launcher derives the sandbox session id at runtime (launcher.go
# `sessionID := reg.ID`), so the container name aileron-sbx-<id>
# (launcher.go:790) is not predictable from the harness. Discover the
# container by its stable name prefix instead.
AILERON_SBX_PREFIX='aileron-sbx-'

# discover_container <prefix>
# Echoes the name of the single running container whose name starts with
# <prefix>. Returns non-zero (and a diagnostic) if zero or more than one
# match, so a stale container from a prior run can never be silently
# probed.
discover_container() {
	prefix="${1:-$AILERON_SBX_PREFIX}"
	names="$(docker ps --filter "name=${prefix}" --format '{{.Names}}' 2>/dev/null)"
	count=0
	match=''
	for n in $names; do
		count=$((count + 1))
		match="$n"
	done
	if [ "$count" -eq 0 ]; then
		printf 'systest: FAIL: no running container named %s* found (is the launch still up?)\n' "$prefix" >&2
		return 1
	fi
	if [ "$count" -gt 1 ]; then
		printf 'systest: FAIL: %d running containers match %s*; expected exactly one:\n%s\n' \
			"$count" "$prefix" "$names" >&2
		return 1
	fi
	printf '%s\n' "$match"
}

# probe_image <container> <expected_image>
# R8.1 — correct image pulled & running. Asserts .Config.Image equals the
# expected ref (caller passes :edge for a dev build, :latest for a release,
# or a digest pin) and the container is running.
probe_image() {
	container="$1"
	expected_image="$2"
	actual_image="$(docker inspect -f '{{.Config.Image}}' "$container" 2>/dev/null)" ||
		{ fail "docker inspect $container failed (probe_image)"; return 1; }
	assert_eq "$expected_image" "$actual_image" "R8.1 container image is $expected_image" || return 1

	running="$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null)"
	assert_eq "true" "$running" "R8.1 container .State.Running" || return 1

	status="$(docker inspect -f '{{.State.Status}}' "$container" 2>/dev/null)"
	assert_eq "running" "$status" "R8.1 container .State.Status" || return 1
}

# probe_mcp_runtime <container>
# R8.2 (agent-agnostic core) — the in-container aileron-mcp binary is present
# and executable (launcher.go:45 sandboxMCPBinPath) and the daemon-wiring env
# vars are all set. These hold for EVERY agent regardless of how the agent is
# pointed at the MCP server (Codex's config.toml, Claude's `--mcp-config`
# flag), so both scenarios reuse this function verbatim. The agent-specific
# proof that the agent is actually wired to aileron-mcp is the caller's job:
# Codex asserts its config file with probe_mcp (which calls this first), Claude
# asserts the `--mcp-config` flag with probe_mcp_cmdline.
probe_mcp_runtime() {
	container="$1"

	# aileron-mcp present and executable (test -x).
	if ! docker exec "$container" test -x /usr/local/bin/aileron-mcp; then
		fail "R8.2 /usr/local/bin/aileron-mcp missing or not executable in $container"
		return 1
	fi
	log "ok: R8.2 /usr/local/bin/aileron-mcp present and executable"

	# Daemon-wiring env vars (launcher.go:609-630). Each must be non-empty.
	for var in AILERON_URL AILERON_COMMS_URL AILERON_SESSION_ID AILERON_APPROVAL_URL AILERON_TOKEN; do
		# `printenv NAME` exits non-zero when unset, so a missing var fails
		# without a brittle string parse.
		if ! docker exec "$container" printenv "$var" >/dev/null 2>&1; then
			fail "R8.2 env $var not set in $container"
			return 1
		fi
		log "ok: R8.2 env $var set"
	done
}

# probe_mcp <container> <agent_config_path> <mcp_block_marker>
# R8.2 (config-file agents, e.g. Codex) — the agent-agnostic core
# (probe_mcp_runtime) PLUS the agent's MCP config file contains the aileron
# MCP block marker. Codex writes /home/agent/.codex/config.toml via
# ConfigureMCP; Claude does not write a config file (it passes `--mcp-config`
# on the command line), so the claude scenario calls probe_mcp_cmdline instead
# of this.
probe_mcp() {
	container="$1"
	config_path="$2"
	mcp_marker="$3"

	probe_mcp_runtime "$container" || return 1

	# Agent MCP config contains the aileron server block.
	config="$(docker exec "$container" cat "$config_path" 2>/dev/null)" ||
		{ fail "R8.2 could not read $config_path in $container"; return 1; }
	assert_contains "$config" "$mcp_marker" "R8.2 $config_path contains $mcp_marker" || return 1
}

# probe_mcp_cmdline <container> <flag> <marker>
# R8.2 (flag-config agents, e.g. Claude) — the agent-agnostic core
# (probe_mcp_runtime) PLUS the agent's MCP wiring is present as a command-line
# flag on the running container's process, not a config file. Claude's
# ConfigureMCP returns `--mcp-config <json>` (claude.go:116-126), appended to
# the container command (launcher.go:1082 `command = append(command,
# extraArgs...)`), so the flag and the aileron MCP server marker both appear in
# the container's `.Config.Cmd` / `.Args`. We assert both the flag and the
# server-name marker so a stray `--mcp-config` with the wrong payload cannot
# pass.
probe_mcp_cmdline() {
	container="$1"
	flag="$2"
	marker="$3"

	probe_mcp_runtime "$container" || return 1

	# The launcher appends extraArgs to the container command; both the entry
	# point Cmd and the resolved Args carry it. Inspect both so we are robust
	# to whichever Docker populates for this image's ENTRYPOINT/CMD shape.
	cmdline="$(docker inspect \
		-f '{{range .Config.Cmd}}{{.}} {{end}}{{range .Args}}{{.}} {{end}}' \
		"$container" 2>/dev/null)" ||
		{ fail "R8.2 docker inspect $container failed (probe_mcp_cmdline)"; return 1; }
	assert_contains "$cmdline" "$flag" "R8.2 container command carries $flag" || return 1
	assert_contains "$cmdline" "$marker" "R8.2 $flag payload references $marker" || return 1
}

# probe_credentials <container> <auth_path>
# R8.3 — credentials mounted & readable. Asserts the auth file exists in the
# container, is mode 0600, and is a bind mount (appears in the container's
# Mounts with Type=bind targeting the auth dir).
probe_credentials() {
	container="$1"
	auth_path="$2"

	if ! docker exec "$container" test -f "$auth_path"; then
		fail "R8.3 auth file $auth_path missing in $container"
		return 1
	fi
	log "ok: R8.3 auth file $auth_path present"

	# Mode 0600. `stat -c %a` is GNU coreutils (the Linux sandbox base);
	# the value is octal without a leading zero.
	mode="$(docker exec "$container" stat -c '%a' "$auth_path" 2>/dev/null)" ||
		{ fail "R8.3 could not stat $auth_path in $container"; return 1; }
	assert_eq "600" "$mode" "R8.3 $auth_path mode is 0600" || return 1

	# The auth file's parent dir is bind-mounted (codex.go AuthSpec uses a
	# parent-dir mount). Assert some bind mount's Destination is a prefix of
	# the auth path.
	auth_dir="$(dirname "$auth_path")"
	binds="$(docker inspect -f '{{range .Mounts}}{{.Type}}:{{.Destination}}{{"\n"}}{{end}}' "$container" 2>/dev/null)"
	case "$binds" in
	*"bind:$auth_dir"*)
		log "ok: R8.3 $auth_dir is a bind mount"
		;;
	*)
		printf 'systest: FAIL: R8.3 no bind mount for %s; mounts were:\n%s\n' "$auth_dir" "$binds" >&2
		return 1
		;;
	esac
}

# probe_daemon_reachable <container>
# R8.4 — daemon reachable from inside. Asserts AILERON_URL's host is
# host.docker.internal (loopback rewrite, launcher.go:845-867) and, on a
# Linux Docker host, that --add-host host.docker.internal:host-gateway is
# present (runtime.go:874-876) so DNS resolves inside the container.
probe_daemon_reachable() {
	container="$1"

	url="$(docker exec "$container" printenv AILERON_URL 2>/dev/null)" ||
		{ fail "R8.4 AILERON_URL not set in $container"; return 1; }
	assert_contains "$url" "host.docker.internal" "R8.4 AILERON_URL host is host.docker.internal" || return 1

	# On Linux Docker, host.docker.internal is not auto-provisioned, so the
	# launcher injects --add-host host.docker.internal:host-gateway. That
	# surfaces in the container's ExtraHosts. On macOS/Windows Docker
	# Desktop the gateway is built in, so ExtraHosts may be empty there —
	# the check is Linux-gated.
	if [ "$(uname -s)" = "Linux" ]; then
		hosts="$(docker inspect -f '{{range .HostConfig.ExtraHosts}}{{.}}{{"\n"}}{{end}}' "$container" 2>/dev/null)"
		assert_contains "$hosts" "host.docker.internal:host-gateway" \
			"R8.4 --add-host host.docker.internal:host-gateway present (Linux)" || return 1
	else
		log "ok: R8.4 host-gateway check skipped (Docker Desktop provides host.docker.internal natively)"
	fi
}

# probe_teardown <prefix>
# R8.5 — clean teardown. Asserts no container matching the sandbox prefix
# remains after the launch returns. The launcher runs `docker run --rm`
# (runtime.go:858), so the container self-removes on exit; SIGINT/SIGTERM
# triggers `docker stop` first (launcher.go:1149-1151). Call this AFTER the
# launch process has exited.
probe_teardown() {
	prefix="${1:-$AILERON_SBX_PREFIX}"
	remaining="$(docker ps -a --filter "name=${prefix}" --format '{{.Names}}' 2>/dev/null)"
	if [ -n "$remaining" ]; then
		printf 'systest: FAIL: R8.5 sandbox container(s) survived teardown:\n%s\n' "$remaining" >&2
		return 1
	fi
	log "ok: R8.5 no sandbox container survived (docker run --rm teardown)"
}

# probe_exit_code <actual_exit> <expected_exit>
# R8.6 — expected exit code of the `aileron launch` process.
probe_exit_code() {
	assert_eq "$2" "$1" "R8.6 aileron launch exit code"
}
