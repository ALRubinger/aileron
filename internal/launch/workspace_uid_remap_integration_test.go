//go:build integration_sandbox

// Workspace uid-remap integration test (#1461).
//
// This test exercises the DAC uid-mismatch contract on the platform where it
// actually breaks: rootful Docker on Linux. `aileron launch --sandbox=docker`
// runs the container as the image's non-root `agent` user, while the workspace
// bind mount is the operator's host CWD owned by the host uid (e.g. 1000). When
// those uids differ, DAC denies the agent write access to the 0755 workspace
// directory ("other" carries no write bit), so the in-container writability
// probe (runtime.go validationScript, the `: > "$probe"` branch) exits 3 and the
// launch is blocked. PR #1460's `:z` SELinux relabel does not address this: it
// is a MAC remedy and a no-op against a DAC mismatch / on daemons without
// SELinux support.
//
// The fix (strategy B): the aileron-remap-agent-uid entrypoint, started as
// root, remaps the in-container `agent` uid/gid to the workspace owner, then
// drops to the agent user via su-exec. This test proves:
//
//   - Negative control: running as the image's default agent user against a
//     workspace owned by a different uid FAILS the writability probe (exit 3).
//   - Positive case: running through aileron-remap-agent-uid + su-exec agent
//     against the same workspace SUCCEEDS — the remap aligned the agent uid to
//     the workspace owner so the agent can write.
//
// The test skips unless GOOS == linux and a Docker runtime is on PATH
// (macOS/Windows Docker Desktop translates uids via its file-sharing shim, so
// the mismatch does not reproduce there), mirroring the AuthSpec bind-mount test.
//
// Run with:
//
//	task test:integration:workspace-uid-remap
//	go test -tags=integration_sandbox -run TestWorkspaceUIDRemap ./internal/launch/...
package launch

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

const workspaceRemapTestImage = "aileron-sandbox-base:workspace-uid-remap-test"

func TestWorkspaceUIDRemap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("workspace DAC uid mismatch only reproduces on Linux; GOOS=%s translates uids via the Docker Desktop shim", runtime.GOOS)
	}
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		t.Skipf("no docker runtime on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildWorkspaceRemapBaseImage(ctx, t, rt)

	runner := sandboxcontainer.DefaultRunner()
	agentUID, err := resolveAgentUID(ctx, runner, rt, workspaceRemapTestImage)
	if err != nil {
		t.Fatalf("resolve agent UID for %s: %v", workspaceRemapTestImage, err)
	}
	if agentUID == 0 {
		t.Fatalf("sandbox-base image resolved agent UID 0 (root); the image's USER directive should select the non-root `agent` user — the workspace DAC contract is meaningless as root")
	}
	hostUID := os.Getuid()
	if hostUID == 0 {
		t.Skipf("host runner is root (UID 0); the negative control needs an unprivileged workspace owner whose uid differs from the container agent uid, which a root runner cannot model")
	}
	if hostUID == agentUID {
		t.Skipf("host UID %d equals container agent UID %d; the DAC mismatch this test guards cannot occur in this environment", hostUID, agentUID)
	}

	// The workspace is the operator's CWD: a 0755 directory owned by the host
	// uid, bind-mounted at WorkspacePath. The probe mirrors the in-container
	// validation script's writability test exactly.
	const probeCmd = "probe=.aileron-remap-probe-$$; : > \"$probe\" && rm -f \"$probe\" && echo writable"

	// Negative control: as the image default agent user, the agent uid differs
	// from the workspace owner, so DAC denies the write and the probe fails.
	t.Run("negative_control_without_remap_fails", func(t *testing.T) {
		hostWorkspace := newWorkspaceDir(t, hostUID)
		stdout, stderr, runErr := runWorkspaceProbe(ctx, rt, hostWorkspace, false, probeCmd)
		if runErr == nil {
			t.Fatalf("expected the in-container write to FAIL without the remap (host UID %d owns the workspace, container runs as agent UID %d), but it succeeded; stdout=%q", hostUID, agentUID, stdout)
		}
		t.Logf("negative control reproduced the DAC denial the remap guards: host UID %d owns the workspace bind mount but the container runs as agent UID %d; the remedy is the aileron-remap-agent-uid entrypoint. runtime stderr: %s", hostUID, agentUID, strings.TrimSpace(stderr))
	})

	// Positive case: routing through aileron-remap-agent-uid (as root) remaps
	// the agent uid to the workspace owner before dropping to the agent user,
	// so the write succeeds.
	t.Run("with_remap_write_succeeds", func(t *testing.T) {
		hostWorkspace := newWorkspaceDir(t, hostUID)
		stdout, stderr, runErr := runWorkspaceProbe(ctx, rt, hostWorkspace, true, probeCmd)
		if runErr != nil {
			t.Fatalf("in-container write FAILED even WITH the remap (host UID %d, agent UID %d): %v\nstderr: %s\nThis is the regression #1461 guards: the entrypoint must remap the agent uid to the workspace owner so the agent can write the operator's CWD.", hostUID, agentUID, runErr, strings.TrimSpace(stderr))
		}
		if got := strings.TrimSpace(stdout); got != "writable" {
			t.Fatalf("in-container probe output = %q, want %q", got, "writable")
		}
	})
}

// buildWorkspaceRemapBaseImage builds the sandbox-base image under a dedicated
// test tag. The build must include the new aileron-remap-agent-uid helper and
// the shadow package (usermod/groupmod), so a green build is itself part of the
// contract.
func buildWorkspaceRemapBaseImage(ctx context.Context, t *testing.T, rt string) {
	t.Helper()
	builder := sandboxcontainer.Builder{
		Runtime: rt,
		Stdout:  testLogWriter{t},
		Stderr:  testLogWriter{t},
	}
	plan := sandboxcomposition.Plan{
		Tier:  sandboxcomposition.TierBase,
		Image: workspaceRemapTestImage,
	}
	if _, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		Plan:   plan,
		Tag:    workspaceRemapTestImage,
		Policy: sandboxcontainer.BuildPolicyAlways,
	}); err != nil {
		t.Fatalf("build sandbox-base test image: %v (set AILERON_SANDBOX_BASE_CONTEXT to images/sandbox-base if the context was not found)", err)
	}
}

// newWorkspaceDir creates a 0755 directory owned by the test-runner uid,
// modelling the operator's CWD that the launcher bind-mounts as the workspace.
func newWorkspaceDir(t *testing.T, ownerUID int) string {
	t.Helper()
	dir := t.TempDir()
	// t.TempDir already creates the dir owned by the test-runner uid with mode
	// 0700; widen to 0755 so the bug is "other has no write bit" rather than
	// "other has no access at all", matching a normal project CWD.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod workspace 0755: %v", err)
	}
	_ = ownerUID // ownership is already the test-runner uid; documented for clarity.
	return dir
}

// runWorkspaceProbe runs a one-shot container with hostWorkspace bind-mounted
// WRITABLE at the sandbox WorkspacePath and runs cmd. When remap is true it
// routes through `aileron-remap-agent-uid su-exec agent /bin/sh -c cmd` as root
// (the production launch chain for the non-proxy path); otherwise it runs as the
// image default agent user. Returns stdout, stderr, and the run error.
func runWorkspaceProbe(ctx context.Context, rt, hostWorkspace string, remap bool, cmd string) (string, string, error) {
	args := []string{
		"run", "--rm",
		"--workdir", sandboxcontainer.WorkspacePath,
		"--volume", hostWorkspace + ":" + sandboxcontainer.WorkspacePath,
	}
	if remap {
		args = append(args, "--user", "0", workspaceRemapTestImage,
			"aileron-remap-agent-uid", "su-exec", "agent", "/bin/sh", "-c", cmd)
	} else {
		args = append(args, workspaceRemapTestImage, "/bin/sh", "-c", cmd)
	}
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, rt, args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	return stdout.String(), stderr.String(), err
}
