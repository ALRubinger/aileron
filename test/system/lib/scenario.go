// Pure scenario decision predicates for the live-Docker probes whose decision
// cores were not yet in probes.go: the R8.3 credentials decision (auth file
// present + mode 0600 + parent dir bind-mounted), the R8.4 daemon-reachable
// decision (AILERON_URL host is host.docker.internal, plus the Linux-gated
// host-gateway ExtraHosts check), and the R8.5 teardown decision (no sandbox
// container survived). Each takes the docker-derived facts the live driver
// gathers (printenv output, stat mode, the mount list, ExtraHosts, surviving
// names) as ordinary parameters and returns the pass/fail error, exactly as
// probes.go does for ProbeMCPRuntime / ProbeExitCode / DiscoverContainer. This
// keeps the orchestration *decisions* unit-tested with stubbed facts while the
// impure docker exec/inspect plumbing stays in the scenario main module.
package systestlib

import (
	"fmt"
	"strings"
)

// ProbeCredentials re-expresses probe_credentials' decision core (R8.3). The live
// driver supplies: authFileExists (`docker exec test -f <auth_path>` succeeded),
// mode (`docker exec stat -c %a <auth_path>`, e.g. "600"), authDir
// (path.Dir(authPath)), and mounts (the `{{.Type}}:{{.Destination}}` block from
// `docker inspect`, one per line). Returns nil only when the file exists, the
// mode is exactly "600", and some bind mount's Destination equals authDir.
// Diagnostics mirror the shell's R8.3 wording so CI logs read the same.
func ProbeCredentials(authFileExists bool, mode, authPath, authDir, mounts string) error {
	if !authFileExists {
		return Fail(fmt.Sprintf("R8.3 auth file %s missing in container", authPath))
	}
	if err := AssertEq("600", mode, fmt.Sprintf("R8.3 %s mode is 0600", authPath)); err != nil {
		return err
	}
	// The auth file's parent dir must be a bind mount. The shell matches the
	// substring `bind:<authDir>` in the mount block; do the same but anchor each
	// line so a parent-of-parent prefix cannot accidentally satisfy it.
	want := "bind:" + authDir
	for _, line := range strings.Split(mounts, "\n") {
		if strings.TrimSpace(line) == want {
			return nil
		}
	}
	return fmt.Errorf("systest: FAIL: R8.3 no bind mount for %s; mounts were:\n%s", authDir, mounts)
}

// ProbeDaemonReachable re-expresses probe_daemon_reachable's decision core
// (R8.4). url is the container's AILERON_URL (`docker exec printenv AILERON_URL`).
// It must contain host.docker.internal (the loopback rewrite). isLinuxHost gates
// the second check: on a Linux Docker host the launcher injects
// `--add-host host.docker.internal:host-gateway`, which surfaces in extraHosts
// (the `{{range .HostConfig.ExtraHosts}}` block); on macOS/Windows Docker Desktop
// the gateway is built in, so the check is skipped. Returns nil when the
// applicable checks hold.
func ProbeDaemonReachable(url string, isLinuxHost bool, extraHosts string) error {
	if err := AssertContains(url, "host.docker.internal", "R8.4 AILERON_URL host is host.docker.internal"); err != nil {
		return err
	}
	if isLinuxHost {
		return AssertContains(extraHosts, "host.docker.internal:host-gateway",
			"R8.4 --add-host host.docker.internal:host-gateway present (Linux)")
	}
	return nil
}

// ProbeTeardown re-expresses probe_teardown's decision core (R8.5): no sandbox
// container survived the launch. survivingNames is the `docker ps -a` names block
// (filtered by the prefix); after whitespace splitting it must be empty. prefix is
// used only for the diagnostic. Returns nil when nothing survived.
func ProbeTeardown(prefix, survivingNames string) error {
	remaining := strings.Fields(survivingNames)
	if len(remaining) == 0 {
		return nil
	}
	return fmt.Errorf("systest: FAIL: R8.5 sandbox container(s) survived teardown:\n%s",
		strings.Join(remaining, "\n"))
}

// ProbeImageRunning re-expresses probe_image's decision core (R8.1) once its
// docker-derived facts are gathered: actualImage (`docker inspect -f
// {{.Config.Image}}`), running (`{{.State.Running}}`, e.g. "true"), and status
// (`{{.State.Status}}`, e.g. "running"). The image-ref arbitration reuses the
// existing ImageRefMatches core. Returns nil when the image matches the expected
// ref (with digest tolerance) and the container is running.
func ProbeImageRunning(expectedImage, actualImage, running, status string) error {
	if !ImageRefMatches(expectedImage, actualImage) {
		return fmt.Errorf(
			"systest: FAIL: R8.1 container image\n  expected: %s (or its @sha256 digest on the same repo)\n  actual:   %s",
			expectedImage, actualImage)
	}
	if err := AssertEq("true", running, "R8.1 container .State.Running"); err != nil {
		return err
	}
	return AssertEq("running", status, "R8.1 container .State.Status")
}

// ProbeMCPConfigContains re-expresses the config-file-specific tail of probe_mcp
// (R8.2 for config-file agents like codex): after the agent-agnostic runtime core
// holds, the agent's MCP config file content must contain the marker. The live
// driver passes config (`docker exec cat <config_path>`), the configPath (for the
// diagnostic), and the marker. Returns nil when the marker is present.
func ProbeMCPConfigContains(config, configPath, marker string) error {
	return AssertContains(config, marker, fmt.Sprintf("R8.2 %s contains %s", configPath, marker))
}
