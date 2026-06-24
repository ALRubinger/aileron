// Pure-logic, docker-free ports of the host-verifiable probe helpers in
// probes.sh. The shell probes inspect a running sandbox container out-of-band
// (`docker inspect` / `docker exec`) and return non-zero with an actionable
// message on a violation. The functions here re-express only the *decision*
// logic that does not need a live container: image-ref arbitration, container
// discovery count arbitration, the exit-code assertion, and the R8.2 MCP
// runtime/cmdline decisions once their docker-derived inputs are passed as
// ordinary Go parameters.
//
// The shell contract test (probes_test.sh) stubs `docker` on PATH and feeds
// those inputs via env (DOCKER_PS_OUTPUT, DOCKER_EXEC_TEST_RC,
// DOCKER_EXEC_PRINTENV_RC, DOCKER_INSPECT_OUTPUT). The Go port replaces the stub
// by passing the same inputs as parameters, so the tests run with no docker
// binary and no stub on PATH.
//
// Mirroring assert.go's contract: a nil error means the asserted condition
// holds; a non-nil error carries the diagnostic (faithful to the shell's stderr
// wording) so a CI failure reads the same as the shell probe's.
//
// The container-bound probes (probe_image, probe_mcp, probe_credentials,
// probe_daemon_reachable, probe_teardown) are intentionally NOT ported here:
// they require a live container and are exercised by the by-hand scenario run,
// exactly as probes_test.sh itself omits them.
package systestlib

import (
	"fmt"
	"strings"
)

// ImageRepo returns the repository portion of an image ref — everything before
// the `:tag` or `@sha256:` digest suffix. `:edge`, `:latest`, and `@sha256:abc…`
// all strip to the same repo; a bare repo (no tag, no digest) returns unchanged.
// A registry `host:port` with no tag (e.g. localhost:5000/aileron-sandbox-codex)
// is preserved, because the trailing `:tag` is only stripped when the final path
// segment (after the last `/`) contains the colon. Pure string logic mirroring
// the shell image_repo, so it is host-verifiable.
func ImageRepo(ref string) string {
	// Strip an `@…` digest suffix first (a digest ref never also carries `:tag`
	// after the `@`, and the repo's registry `host:port` colon precedes the `@`).
	if before, _, found := strings.Cut(ref, "@"); found {
		ref = before
	}
	// Strip a trailing `:tag`. Guard against a registry `host:port` with no tag
	// by only stripping when the final path segment (after the last `/`) carries
	// the colon.
	last := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		last = ref[i+1:]
	}
	if strings.Contains(last, ":") {
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			ref = ref[:i]
		}
	}
	return ref
}

// ImageRefMatches reports R8.1 image equality with digest tolerance, mirroring
// image_ref_matches. The launcher resolves a floating expected ref (e.g.
// …-codex:edge) to whatever it pulled: the same floating tag on a dev build, or
// a `…-codex@sha256:…` digest pin on a reproducible release (#1233). A match
// requires the SAME repository plus either a floating tag (`:edge`/`:latest`) or
// an `@sha256:` digest, so a pinned release no longer fails the probe while a
// wrong repo/agent still does. An explicit expected ref (e.g. a caller-supplied
// digest or other EXPECTED_IMAGE override) matches exactly. Pure predicate;
// host-verifiable.
func ImageRefMatches(expected, actual string) bool {
	expRepo := ImageRepo(expected)
	actRepo := ImageRepo(actual)
	if expRepo != actRepo {
		return false
	}
	// Accept a floating tag or an @sha256: digest on the matched repo.
	if actual == actRepo+":edge" || actual == actRepo+":latest" {
		return true
	}
	if strings.HasPrefix(actual, actRepo+"@sha256:") {
		return true
	}
	// Exact match against the expected ref (covers a caller-supplied digest or
	// any other explicit EXPECTED_IMAGE override).
	return expected == actual
}

// SplitNames splits a raw, newline/whitespace-delimited block of candidate
// container names (e.g. the `docker ps --format '{{.Names}}'` output the shell
// feeds via DOCKER_PS_OUTPUT) into individual names, dropping empty entries.
// This mirrors the shell `for n in $names` word-splitting so a caller can feed
// DiscoverContainer either a pre-split []string or the raw block via this
// helper.
func SplitNames(raw string) []string {
	return strings.Fields(raw)
}

// DiscoverContainer arbitrates the running-container match count exactly as the
// shell discover_container does over its `docker ps` output. Whitespace-only
// entries are ignored (mirroring shell word-splitting). Exactly one non-empty
// name returns that name and a nil error; zero matches returns a non-nil error
// (never a silent empty string) so a stale container can never be silently
// probed; more than one match returns a non-nil error rather than guessing.
// prefix is used only for the diagnostic wording, matching the shell.
func DiscoverContainer(prefix string, names []string) (string, error) {
	matches := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n != "" {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf(
			"systest: FAIL: no running container named %s* found (is the launch still up?)",
			prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf(
			"systest: FAIL: %d running containers match %s*; expected exactly one:\n%s",
			len(matches), prefix, strings.Join(matches, "\n"))
	}
}

// ProbeExitCode mirrors probe_exit_code: nil when the actual `aileron launch`
// exit code equals expected, and a non-nil R8.6 diagnostic (the assert_eq
// message shape) when they differ. Int parameters are the honest contract for
// exit codes.
func ProbeExitCode(actual, expected int) error {
	return AssertEq(fmt.Sprintf("%d", expected), fmt.Sprintf("%d", actual), "R8.6 aileron launch exit code")
}

// ProbeMCPRuntime re-expresses the agent-agnostic R8.2 core of
// probe_mcp_runtime. The shell derives two facts from a live container and feeds
// them here as parameters: binaryExecutable is whether `test -x
// /usr/local/bin/aileron-mcp` succeeded, and envVarsAllSet is whether every
// daemon-wiring env var was present (the shell loops `printenv` over
// AILERON_URL, AILERON_COMMS_URL, AILERON_SESSION_ID, AILERON_APPROVAL_URL,
// AILERON_TOKEN, but the stub feeds one rc for all, so the contract under test
// is "all set / not all set"). The binary check gates before the env check
// (faithful ordering), so a missing binary yields the binary diagnostic even
// when env is also unset. Returns nil only when both hold.
func ProbeMCPRuntime(binaryExecutable, envVarsAllSet bool) error {
	if !binaryExecutable {
		return Fail("R8.2 /usr/local/bin/aileron-mcp missing or not executable")
	}
	if !envVarsAllSet {
		return Fail("R8.2 daemon-wiring env var not set")
	}
	return nil
}

// ProbeMCPCmdline re-expresses probe_mcp_cmdline for flag-config agents (e.g.
// Claude). The agent-agnostic core (ProbeMCPRuntime) gates first, so a runtime
// failure (missing binary or unset env) fails the whole probe before the
// cmdline is inspected. Then the inspected container command (cmdline) must
// contain both flag (e.g. `--mcp-config`) and marker (e.g. the aileron server
// name), so a stray flag with the wrong payload cannot pass. Diagnostics mirror
// the shell's R8.2 wording.
func ProbeMCPCmdline(binaryExecutable, envVarsAllSet bool, cmdline, flag, marker string) error {
	if err := ProbeMCPRuntime(binaryExecutable, envVarsAllSet); err != nil {
		return err
	}
	if !strings.Contains(cmdline, flag) {
		return Fail(fmt.Sprintf("R8.2 container command carries %s", flag))
	}
	if !strings.Contains(cmdline, marker) {
		return Fail(fmt.Sprintf("R8.2 %s payload references %s", flag, marker))
	}
	return nil
}
