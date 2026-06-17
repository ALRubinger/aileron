//go:build integration_sandbox

// Per-agent published image launchability smoke (#1086).
//
// Where sandbox_features_composition_integration_test.go (#1083) builds a
// features-composed image inside the test and validates it, this test takes an
// ALREADY-BUILT per-agent image — the `:smoke` single-arch load produced by the
// sandbox-agents.yml workflow on PR, or the freshly PUBLISHED manifest pulled
// back on workflow_dispatch — and asserts it is launchable exactly as the
// launcher validates it.
//
// The image ref and the agent command are passed via env so one test body
// serves both matrix legs (claude, codex) and both CI paths (loaded `:smoke`
// vs. pulled published manifest):
//
//	AILERON_SANDBOX_AGENT_IMAGE    fully-qualified image ref to validate
//	AILERON_SANDBOX_AGENT_COMMAND  agent CLI that must resolve on PATH (claude/codex)
//
// The smoke mirrors the #1121/#1083 launchability path: assert the agent CLI is
// on PATH, then run Builder.Validate with RequireProxyTrust=false and
// RequireMCPBinary=false. Both gates are unset because this composed image
// derives from the base WITHOUT a launch-time ca.pem mounted and the host-mount
// is not in play for the smoke; the launchability contract under test is a
// writable /home/agent/workspace, the correct CWD, and the agent on PATH.
//
// Per the umbrella's two-layer test posture this test is FAIL-FAST: there is no
// t.Skip. If Docker, the env, or the image is absent, the test FAILS. CI
// provisions the toolchain and sets the env per matrix agent.
//
// Run with:
//
//	task test:integration:sandbox-agent-image
//
// or directly:
//
//	AILERON_SANDBOX_AGENT_IMAGE=... AILERON_SANDBOX_AGENT_COMMAND=claude \
//	  go test -tags=integration_sandbox -run TestSandboxAgentImageSmoke ./launch/...
package launch

import (
	"context"
	"os"
	"testing"
	"time"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// TestSandboxAgentImageSmoke validates that the per-agent image named by
// AILERON_SANDBOX_AGENT_IMAGE is launchable: the agent CLI named by
// AILERON_SANDBOX_AGENT_COMMAND is on PATH, and Builder.Validate succeeds with
// both the proxy-trust and MCP-binary gates unset.
func TestSandboxAgentImageSmoke(t *testing.T) {
	image := os.Getenv("AILERON_SANDBOX_AGENT_IMAGE")
	if image == "" {
		// Fail-fast: CI sets the image ref per matrix agent; absence is a job
		// configuration failure, not a skip.
		t.Fatalf("AILERON_SANDBOX_AGENT_IMAGE must be set to the per-agent image ref under test (required, not skipped)")
	}
	command := os.Getenv("AILERON_SANDBOX_AGENT_COMMAND")
	if command == "" {
		t.Fatalf("AILERON_SANDBOX_AGENT_COMMAND must be set to the agent CLI (claude/codex) expected on PATH (required, not skipped)")
	}

	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		// Fail-fast: CI provisions Docker; absence is a job failure, not a skip.
		t.Fatalf("no docker runtime on PATH (required, not skipped): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// (a) The agent Feature put the CLI on PATH for the running user.
	assertCommandOnPath(ctx, t, rt, image, command)

	// (b) Launchability: the image satisfies the launch-time runtime contract,
	// not merely carrying the agent CLI on PATH. Builder.Validate runs the same
	// probe the launcher uses — a writable /home/agent/workspace mount, CWD
	// there, and the agent command resolvable.
	builder := sandboxcontainer.Builder{
		Stdout: testLogWriter{t},
		Stderr: testLogWriter{t},
	}

	// The workspace is bind-mounted at /home/agent/workspace and the container
	// runs as the non-root `agent` user, whose uid does not match the host uid
	// that owns t.TempDir() (0700). On a Linux Docker host that makes the mount
	// unwritable to `agent`, so Validate's writable-workspace probe fails; make
	// it world-writable so the launchability check exercises the IMAGE contract,
	// not host/container uid mapping. (Docker Desktop masks this with permissive
	// file sharing, which is why it only surfaces on Linux CI.)
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o777); err != nil {
		t.Fatalf("chmod validate workspace: %v", err)
	}

	// RequireProxyTrust and RequireMCPBinary are both false: the composed image
	// derives from the base WITHOUT a launch-time ca.pem mounted, so the proxy
	// gate would fail deterministically, and the MCP host-mount is not exercised
	// by this image-only smoke (#957/ADR-0024). The launchability contract under
	// test is the writable workspace, CWD, and agent-on-PATH only.
	if err := builder.Validate(ctx, sandboxcontainer.ValidateOptions{
		Runtime:           rt,
		Image:             image,
		WorkDir:           workspace,
		Command:           []string{command},
		RequireProxyTrust: false,
		RequireMCPBinary:  false,
	}); err != nil {
		t.Fatalf("Builder.Validate (launchability of published per-agent image %s): %v", image, err)
	}
}
