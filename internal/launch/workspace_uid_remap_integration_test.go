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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

	// Regression for #1467: the remap must NOT trigger shadow's non-prune-aware
	// home-tree chown. Before the fix, the helper called `usermod -u`, whose
	// built-in ownership walk descended into the bind-mounted workspace and
	// printed "usermod: Failed to change ownership of the home directory". The
	// fix rewrites /etc/passwd and /etc/group directly, so no such walk runs.
	// The remap stderr must be free of any usermod chown-failure noise while the
	// workspace stays writable.
	t.Run("remap_emits_no_home_chown_failure", func(t *testing.T) {
		hostWorkspace := newWorkspaceDir(t, hostUID)
		stdout, stderr, runErr := runWorkspaceProbe(ctx, rt, hostWorkspace, true, probeCmd)
		if runErr != nil {
			t.Fatalf("remap probe failed (host UID %d, agent UID %d): %v\nstderr: %s", hostUID, agentUID, runErr, strings.TrimSpace(stderr))
		}
		if got := strings.TrimSpace(stdout); got != "writable" {
			t.Fatalf("remap probe output = %q, want %q (workspace must stay writable)", got, "writable")
		}
		for _, marker := range []string{"usermod", "Failed to change ownership"} {
			if strings.Contains(stderr, marker) {
				t.Fatalf("remap stderr contained %q, indicating shadow's non-prune-aware home-tree chown ran (regression #1467):\n%s", marker, strings.TrimSpace(stderr))
			}
		}
	})

	// Regression for #1467: the remap must leave the operator's host files inside
	// the bind-mounted workspace untouched. A pre-existing file the operator
	// created in the workspace must retain its host ownership after the remap —
	// proving the prune-aware re-chown excluded the workspace and that shadow's
	// recursive home chown (which would rewrite the host project's ownership) did
	// not run.
	t.Run("remap_leaves_host_workspace_files_untouched", func(t *testing.T) {
		hostWorkspace := newWorkspaceDir(t, hostUID)
		hostFile := filepath.Join(hostWorkspace, "operator-file.txt")
		if err := os.WriteFile(hostFile, []byte("host content\n"), 0o644); err != nil {
			t.Fatalf("seed host workspace file: %v", err)
		}
		before, err := os.Stat(hostFile)
		if err != nil {
			t.Fatalf("stat seeded host file: %v", err)
		}
		beforeUID := before.Sys().(*syscall.Stat_t).Uid

		// Run the remap; the command is incidental, the side effect on host
		// ownership is what we assert.
		_, stderr, runErr := runWorkspaceProbe(ctx, rt, hostWorkspace, true, "echo done")
		if runErr != nil {
			t.Fatalf("remap probe failed: %v\nstderr: %s", runErr, strings.TrimSpace(stderr))
		}

		after, err := os.Stat(hostFile)
		if err != nil {
			t.Fatalf("stat host file after remap: %v", err)
		}
		afterUID := after.Sys().(*syscall.Stat_t).Uid
		if afterUID != beforeUID {
			t.Fatalf("host workspace file ownership changed by the remap: uid %d -> %d (regression #1467: the workspace bind mount must be pruned from the re-chown and shadow's recursive home chown must not run)", beforeUID, afterUID)
		}
	})

	// Regression for #1467 (facet 2): the launcher bind-mounts read-only config
	// files directly under /home/agent (.claude.json, .gitconfig today; codex's
	// config tomorrow), owned by the image agent uid. The remap's re-chown of the
	// home scaffolding must tolerate them: chowning a read-only mount fails EROFS,
	// which under the helper's `set -e` aborted the entrypoint before the agent
	// started (exit 1, "chown: ... Read-only file system"). The fix tolerates the
	// failed chown (the re-chown suppresses per-entry errors). With such a mount
	// present the probe must still succeed and the remap stderr must carry no
	// chown error.
	t.Run("remap_tolerates_readonly_home_mounts", func(t *testing.T) {
		hostWorkspace := newWorkspaceDir(t, hostUID)
		// A read-only file at its real launcher target under /home/agent. The
		// re-chown walks all of /home/agent (except the pruned workspace), so it
		// attempts to chown this mount; pre-fix that EROFS aborted the launch.
		roFile := seedAgentOwnedFile(ctx, t, rt, ".claude.json")
		mount := roFile + ":/home/agent/.claude.json:ro"
		stdout, stderr, runErr := runWorkspaceProbe(ctx, rt, hostWorkspace, true, probeCmd, mount)
		if runErr != nil {
			t.Fatalf("remap probe failed with a read-only home mount present (host UID %d, agent UID %d): %v\nstderr: %s\nRegression #1467 facet 2: the re-chown must tolerate read-only launcher bind mounts under /home/agent rather than abort the entrypoint.", hostUID, agentUID, runErr, strings.TrimSpace(stderr))
		}
		if got := strings.TrimSpace(stdout); got != "writable" {
			t.Fatalf("remap probe output = %q, want %q (workspace must stay writable with a read-only home mount present)", got, "writable")
		}
		for _, marker := range []string{"chown:", "Read-only file system"} {
			if strings.Contains(stderr, marker) {
				t.Fatalf("remap stderr contained %q, indicating the re-chown choked on the read-only home mount (regression #1467 facet 2):\n%s", marker, strings.TrimSpace(stderr))
			}
		}
	})

	// Regression for #1467 (cache-volume facet caught in review): the launcher
	// mounts fresh Docker named cache volumes under /home/agent that come up
	// ROOT-owned; the remap must re-chown them to the new agent id so the
	// non-root agent can use its cache. An owner-scoped re-chown would skip a
	// root-owned mount and silently break the cache, so the re-chown must NOT be
	// owner-scoped. Assert the remapped agent can WRITE a root-owned writable
	// mount under /home/agent.
	t.Run("remap_rechowns_root_owned_writable_mount", func(t *testing.T) {
		hostWorkspace := newWorkspaceDir(t, hostUID)
		cacheDir := seedRootOwnedDir(ctx, t, rt)
		// Writable (no :ro), at a cache-like path under /home/agent.
		mount := cacheDir + ":/home/agent/.cache/aileron-remap-test"
		cmd := "probe=/home/agent/.cache/aileron-remap-test/.w-$$; : > \"$probe\" && rm -f \"$probe\" && echo writable"
		stdout, stderr, runErr := runWorkspaceProbe(ctx, rt, hostWorkspace, true, cmd, mount)
		if runErr != nil {
			t.Fatalf("remap probe could not write a root-owned mount under /home/agent (host UID %d, agent UID %d): %v\nstderr: %s\nRegression #1467: the remap must re-chown root-owned writable mounts (fresh named cache volumes) to the agent; an owner-scoped re-chown would skip them.", hostUID, agentUID, runErr, strings.TrimSpace(stderr))
		}
		if got := strings.TrimSpace(stdout); got != "writable" {
			t.Fatalf("remap probe output = %q, want %q (agent must be able to write a remapped root-owned mount)", got, "writable")
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

// runRootInSeedDir runs `script` as root in a throwaway container with dir
// bind-mounted at /seed. It exists because the unprivileged test runner cannot
// set up fixture ownership itself (chown to a foreign uid/gid).
func runRootInSeedDir(ctx context.Context, t *testing.T, rt, dir, script string) {
	t.Helper()
	var out bytes.Buffer
	c := exec.CommandContext(ctx, rt,
		"run", "--rm", "--user", "0",
		"--volume", dir+":/seed",
		workspaceRemapTestImage,
		"/bin/sh", "-c", script,
	)
	c.Stdout = &out
	c.Stderr = &out
	if err := c.Run(); err != nil {
		t.Fatalf("root seed container failed (%s): %v\n%s", script, err, strings.TrimSpace(out.String()))
	}
}

// seedAgentOwnedFile creates a 0644 file named `name`, owned by the image agent
// user, inside a fresh 0755 temp dir, and returns its host path. `chown
// agent:agent` resolves the agent user's numeric uid:gid from the image's own
// passwd/group. This models the launcher's read-only config mounts
// (.claude.json/.gitconfig), which it owns to the image agent user.
func seedAgentOwnedFile(ctx context.Context, t *testing.T, rt, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod seed dir 0755: %v", err)
	}
	runRootInSeedDir(ctx, t, rt, dir, fmt.Sprintf(
		"printf '{}\\n' > /seed/%[1]s && chown agent:agent /seed/%[1]s && chmod 0644 /seed/%[1]s", name))
	return filepath.Join(dir, name)
}

// seedRootOwnedDir returns a fresh 0755 temp dir chowned to root:root via a root
// container, modelling a fresh Docker named cache volume (which comes up
// root-owned when its path is not pre-created in the image). It registers a
// cleanup that restores test-runner ownership so the temp-dir RemoveAll — run by
// the unprivileged runner — can delete the tree even if the remap (or a failing
// assertion) left it root-owned.
func seedRootOwnedDir(ctx context.Context, t *testing.T, rt string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod seed dir 0755: %v", err)
	}
	runRootInSeedDir(ctx, t, rt, dir, "chown 0:0 /seed")
	t.Cleanup(func() {
		runRootInSeedDir(context.WithoutCancel(ctx), t, rt, dir,
			fmt.Sprintf("chown -R %d:%d /seed", os.Getuid(), os.Getgid()))
	})
	return dir
}

// runWorkspaceProbe runs a one-shot container with hostWorkspace bind-mounted
// WRITABLE at the sandbox WorkspacePath and runs cmd. When remap is true it
// routes through `aileron-remap-agent-uid su-exec agent /bin/sh -c cmd` as root
// (the production launch chain for the non-proxy path); otherwise it runs as the
// image default agent user. Returns stdout, stderr, and the run error.
func runWorkspaceProbe(ctx context.Context, rt, hostWorkspace string, remap bool, cmd string, extraMounts ...string) (string, string, error) {
	args := []string{
		"run", "--rm",
		"--workdir", sandboxcontainer.WorkspacePath,
		"--volume", hostWorkspace + ":" + sandboxcontainer.WorkspacePath,
	}
	for _, m := range extraMounts {
		args = append(args, "--volume", m)
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
