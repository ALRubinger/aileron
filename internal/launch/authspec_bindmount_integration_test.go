//go:build integration_sandbox

// AuthSpec bind-mount writability integration test (#988).
//
// This test exercises the load-bearing AuthSpec lifecycle contract on
// the platform where it actually breaks: rootful-Docker Linux. The
// launcher renders agent credentials into a transient host directory
// (chmod 0700, owned by the host operator's UID), then bind-mounts that
// directory WRITABLE into the sandbox container at the binding's parent
// (e.g. /home/agent/.claude/). Claude rotates .credentials.json
// mid-session by writing a tmpfile and renaming it over the original,
// which requires the in-container agent user to own the mounted
// directory and its files. The container runs as the image's `agent`
// system user (created by BusyBox `adduser -S`, UID assigned
// dynamically), not as the host operator's UID. A writable bind mount
// preserves host ownership, so without the chown-to-agent-UID fix the
// in-container write fails with EPERM.
//
// The test:
//
//   - Skips unless GOOS == linux and a Docker runtime is on PATH
//     (macOS/Windows Docker Desktop translates UIDs via its file-sharing
//     shim, so the bug does not reproduce there).
//   - Builds the aileron/sandbox-base image (Tier base).
//   - Creates a transient dir exactly as prepareAuthSpec does and seeds
//     a credential file.
//   - Negative control: WITHOUT the chown fix, an in-container write as
//     the agent user must fail; on that failure the test surfaces the
//     permanent diagnostic naming the UID mismatch and the chown fix.
//   - Positive case: WITH the chown fix applied (resolveAgentUID +
//     recursive chown), the same write succeeds (container exit 0) and
//     the host-side file reflects the in-container write.
//
// Run with:
//
//	go test -tags=integration_sandbox -run TestAuthSpecBindMountWritable ./internal/launch/...
package launch

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

const authSpecBindMountTestImage = "aileron/sandbox-base:authspec-bindmount-test"

func TestAuthSpecBindMountWritable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("AuthSpec bind-mount UID mismatch only reproduces on Linux; GOOS=%s translates UIDs via the Docker Desktop shim", runtime.GOOS)
	}
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		t.Skipf("no docker runtime on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAuthSpecBaseImage(ctx, t, rt)

	runner := sandboxcontainer.DefaultRunner()
	agentUID, err := resolveAgentUID(ctx, runner, rt, authSpecBindMountTestImage)
	if err != nil {
		t.Fatalf("resolve agent UID for %s: %v", authSpecBindMountTestImage, err)
	}
	if agentUID == 0 {
		t.Fatalf("sandbox-base image resolved agent UID 0 (root); the image's USER directive should select the non-root `agent` user — the bind-mount writability contract is meaningless as root")
	}
	hostUID := os.Getuid()
	if hostUID == 0 {
		// Negative control guard. The container still runs as the non-root
		// image `agent` user (runAsAgent does not pass --user 0), so the
		// EPERM the test reproduces fires even when the host runner is root;
		// a root host does NOT bypass the in-container DAC check. We skip
		// because a root host does not model the unprivileged operator the
		// AuthSpec lifecycle targets — the negative control's value is
		// reproducing the operator's reality, and a root runner is not it.
		t.Skipf("host runner is root (UID 0); the negative control runs the container as the non-root image `agent` user so EPERM still fires regardless of host root, but a root host does not model the unprivileged operator the AuthSpec lifecycle targets, so the control is run only on a non-root host")
	}
	if hostUID == agentUID {
		t.Skipf("host UID %d equals container agent UID %d; the EPERM mismatch this test guards cannot occur in this environment", hostUID, agentUID)
	}

	// Container path mirrors prepareAuthSpec's mount target for Claude.
	const containerParent = "/home/agent/.claude"
	const containerFile = containerParent + "/.credentials.json"
	// Reproduce Claude's mid-session rotation precisely: write a tmpfile
	// in the mounted dir then rename it over .credentials.json. Both the
	// tmpfile create and the rename require the agent to OWN the mounted
	// directory (creating a new entry and unlinking the old one are
	// directory operations), which is exactly what the chown fix grants.
	// Read the rotated file back so a success path is observable.
	const writeCmd = "echo rotated > " + containerParent + "/.credentials.json.tmp && " +
		"mv " + containerParent + "/.credentials.json.tmp " + containerFile + " && " +
		"cat " + containerFile

	// Negative control: without the chown fix, the host operator owns the
	// dir and the agent user cannot perform the tmpfile+rename rotation.
	t.Run("negative_control_without_chown_fails", func(t *testing.T) {
		hostRoot := newTransientAuthDir(t, containerParent)
		stdout, stderr, runErr := runAsAgent(ctx, rt, hostRoot, containerParent, writeCmd)
		if runErr == nil {
			t.Fatalf("expected the in-container write to FAIL without the chown fix (host UID %d owns the dir, container runs as agent UID %d), but it succeeded; stdout=%q", hostUID, agentUID, stdout)
		}
		// Permanent diagnostic per #988 acceptance: name the UID mismatch
		// and point at the chown-to-agent-UID fix.
		t.Logf("negative control reproduced the EPERM the fix guards: host UID %d owns the AuthSpec bind-mount but the container runs as agent UID %d; the remedy is the chown-to-agent-UID fix applied before the writable mount. runtime stderr: %s", hostUID, agentUID, strings.TrimSpace(stderr))
	})

	// Positive case: with the chown fix, the agent owns the tree and the
	// write succeeds; the host-side file reflects it. The chown runs the
	// same way production does (chownTreeViaRuntime): a root container
	// bind-mounts the tree and chowns it, because the unprivileged
	// test-runner UID cannot chown host files to the foreign agent UID.
	t.Run("with_chown_fix_write_succeeds", func(t *testing.T) {
		hostRoot := newTransientAuthDir(t, containerParent)
		if err := chownTreeViaRuntime(ctx, runner, rt, authSpecBindMountTestImage, hostRoot, agentUID); err != nil {
			t.Fatalf("apply chown fix (recursive chown to agent UID %d via runtime): %v", agentUID, err)
		}
		stdout, stderr, runErr := runAsAgent(ctx, rt, hostRoot, containerParent, writeCmd)
		if runErr != nil {
			t.Fatalf("in-container write FAILED even WITH the chown-to-agent-UID fix (host UID %d, agent UID %d): %v\nstderr: %s\nThis is the regression #988 guards: the AuthSpec writable bind mount must be chowned to the resolved image agent UID so the agent can rotate credentials.", hostUID, agentUID, runErr, strings.TrimSpace(stderr))
		}
		if got := strings.TrimSpace(stdout); got != "rotated" {
			t.Fatalf("in-container read-back = %q, want %q", got, "rotated")
		}
		// The agent rotated the file as the agent UID, so it is now owned by
		// that UID (mode 0600). Reclaim ownership to the test-runner UID via
		// the privileged runtime path, exactly as capture-on-exit does,
		// before the host-side read. Without this the unprivileged read
		// fails with EPERM and the rotation would be silently lost. This
		// also restores ownership so t.TempDir's cleanup can remove the tree.
		if err := chownTreeViaRuntime(ctx, runner, rt, authSpecBindMountTestImage, hostRoot, os.Getuid()); err != nil {
			t.Fatalf("reclaim ownership to host UID %d before capture read: %v", os.Getuid(), err)
		}
		// Host-side file must reflect the in-container write so the
		// launcher's capture-on-exit can snapshot it back to the vault.
		hostFile := filepath.Join(hostRoot, filepath.Base(containerFile))
		hostBytes, err := os.ReadFile(hostFile)
		if err != nil {
			t.Fatalf("read host-side file after reclaim: %v", err)
		}
		if got := strings.TrimSpace(string(hostBytes)); got != "rotated" {
			t.Fatalf("host-side file = %q after in-container rotation, want %q", got, "rotated")
		}
	})
}

// buildAuthSpecBaseImage builds (or rebuilds) the sandbox-base image
// under a dedicated test tag so the test never collides with a developer's
// working image. AILERON_SANDBOX_BASE_CONTEXT may point at the
// images/sandbox-base context; otherwise the Builder walks up from cwd.
func buildAuthSpecBaseImage(ctx context.Context, t *testing.T, rt string) {
	t.Helper()
	builder := sandboxcontainer.Builder{
		Runtime: rt,
		Stdout:  testLogWriter{t},
		Stderr:  testLogWriter{t},
	}
	plan := sandboxcomposition.Plan{
		Tier:  sandboxcomposition.TierBase,
		Image: authSpecBindMountTestImage,
	}
	if _, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		Plan:   plan,
		Tag:    authSpecBindMountTestImage,
		Policy: sandboxcontainer.BuildPolicyAlways,
	}); err != nil {
		t.Fatalf("build sandbox-base test image: %v (set AILERON_SANDBOX_BASE_CONTEXT to images/sandbox-base if the context was not found)", err)
	}
}

// newTransientAuthDir reproduces prepareAuthSpec's transient-dir setup:
// a 0700 temp dir owned by the test-runner UID, with the container
// parent's subdirectory mirrored beneath it and a seed file written.
// It returns the host-side directory that maps to containerParent.
func newTransientAuthDir(t *testing.T, containerParent string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod transient root 0700: %v", err)
	}
	rel := strings.TrimLeft(containerParent, "/")
	hostDir := filepath.Join(root, rel)
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		t.Fatalf("mkdir host group dir: %v", err)
	}
	// Seed a placeholder so the directory is non-empty, mirroring a
	// pre-seeded credential the agent later rotates.
	if err := os.WriteFile(filepath.Join(hostDir, ".credentials.json"), []byte("seed"), 0o600); err != nil {
		t.Fatalf("seed credential file: %v", err)
	}
	return hostDir
}

// runAsAgent runs a one-shot container as the image's default (agent)
// user with hostDir bind-mounted WRITABLE at containerParent, executing
// cmd via /bin/sh -c. It returns stdout, stderr, and the run error.
func runAsAgent(ctx context.Context, rt, hostDir, containerParent, cmd string) (string, string, error) {
	args := []string{
		"run", "--rm",
		"--volume", hostDir + ":" + containerParent,
		authSpecBindMountTestImage,
		"/bin/sh", "-c", cmd,
	}
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, rt, args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	return stdout.String(), stderr.String(), err
}

// testLogWriter routes container build output to the test log.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}
