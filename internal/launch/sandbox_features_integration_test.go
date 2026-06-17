//go:build integration_sandbox

// Sandbox Feature build e2e test (#1082).
//
// This test exercises the load-bearing contract of the per-agent
// devcontainer Features under images/sandbox-features/<agent>/: composing the
// Aileron sandbox base with a single agent Feature must produce an image where
// that agent's CLI is on PATH for the running user.
//
// It drives @devcontainers/cli directly (NOT Aileron's own
// composition.Discover build path — that wiring is #1083's scope):
//
//   - Build the aileron/sandbox-base image (Tier base) via the Builder.
//   - Generate a minimal devcontainer.json that FROMs the freshly built base
//     and references the local Feature directory by path.
//   - `devcontainer build` the composed image.
//   - `docker run` the built image and assert `command -v <cli>` exits 0.
//
// Per #1082 this test is FAIL-FAST: there is no t.Skip. If Go, Node,
// @devcontainers/cli, Docker, or the base image is absent, the test FAILS.
// CI provisions the toolchain (see the integration-sandbox-features job).
//
// Run with:
//
//	task test:integration:sandbox-features
//
// or directly:
//
//	go test -tags=integration_sandbox -run TestSandboxFeatures ./launch/...
package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

const sandboxFeaturesBaseImage = "aileron/sandbox-base:features-test"

// sandboxFeatureCase is one agent's build-and-verify contract.
type sandboxFeatureCase struct {
	agent   string // Feature id / directory name under images/sandbox-features
	command string // the CLI binary that must be on PATH after the build
}

func TestSandboxFeatures(t *testing.T) {
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		// Fail-fast per #1082: CI provisions Docker; absence is a job failure,
		// not a skip.
		t.Fatalf("no docker runtime on PATH (required, not skipped): %v", err)
	}
	requireDevcontainersCLI(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildSandboxFeaturesBaseImage(ctx, t, rt)

	featuresRoot := sandboxFeaturesContext(t)

	cases := []sandboxFeatureCase{
		{agent: "claude", command: "claude"},
		{agent: "codex", command: "codex"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.agent, func(t *testing.T) {
			featureDir := filepath.Join(featuresRoot, tc.agent)
			if _, err := os.Stat(filepath.Join(featureDir, "devcontainer-feature.json")); err != nil {
				t.Fatalf("feature %q not found at %s: %v", tc.agent, featureDir, err)
			}
			image := buildComposedImage(ctx, t, featureDir, tc.agent)
			assertCommandOnPath(ctx, t, rt, image, tc.command)
		})
	}
}

// requireDevcontainersCLI fails the test (does not skip) if the
// @devcontainers/cli is not invokable.
func requireDevcontainersCLI(t *testing.T) {
	t.Helper()
	cmd := exec.Command("devcontainer", "--version")
	if err := cmd.Run(); err != nil {
		t.Fatalf("@devcontainers/cli not on PATH (required, not skipped): %v; install with `npm install -g @devcontainers/cli`", err)
	}
}

// sandboxFeaturesContext resolves the images/sandbox-features directory,
// honoring AILERON_SANDBOX_FEATURES_CONTEXT, otherwise walking up from cwd.
func sandboxFeaturesContext(t *testing.T) string {
	t.Helper()
	if env := strings.TrimSpace(os.Getenv("AILERON_SANDBOX_FEATURES_CONTEXT")); env != "" {
		return env
	}
	start, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := start
	for {
		candidate := filepath.Join(dir, "images", "sandbox-features")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("images/sandbox-features context not found from %s; set AILERON_SANDBOX_FEATURES_CONTEXT", start)
		}
		dir = parent
	}
}

// buildSandboxFeaturesBaseImage builds the sandbox-base image under a
// dedicated test tag so the composed Features FROM a known-good substrate.
func buildSandboxFeaturesBaseImage(ctx context.Context, t *testing.T, rt string) {
	t.Helper()
	builder := sandboxcontainer.Builder{
		Runtime: rt,
		Stdout:  testLogWriter{t},
		Stderr:  testLogWriter{t},
	}
	plan := sandboxcomposition.Plan{
		Tier:  sandboxcomposition.TierBase,
		Image: sandboxFeaturesBaseImage,
	}
	if _, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		Plan:   plan,
		Tag:    sandboxFeaturesBaseImage,
		Policy: sandboxcontainer.BuildPolicyAlways,
	}); err != nil {
		t.Fatalf("build sandbox-base test image: %v (set AILERON_SANDBOX_BASE_CONTEXT to images/sandbox-base if the context was not found)", err)
	}
}

// buildComposedImage writes a minimal devcontainer.json that FROMs the test
// base image and references the local Feature directory by a RELATIVE path,
// then runs `devcontainer build` and returns the resulting image id.
//
// @devcontainers/cli rejects an absolute path as a feature id ("An Absolute
// path to a local feature is not allowed"). Local Features must live beneath
// the workspace's .devcontainer/ and be referenced relative to it (e.g.
// "./claude"). The source Feature is copied into the transient workspace so
// the test never mutates the repo tree.
func buildComposedImage(ctx context.Context, t *testing.T, featureDir, agent string) string {
	t.Helper()
	workspace := t.TempDir()
	devDir := filepath.Join(workspace, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}

	// Copy the Feature into .devcontainer/<agent>/ so it can be referenced by
	// the only path form the CLI accepts: relative to .devcontainer.
	localFeatureDir := filepath.Join(devDir, agent)
	copyFeatureDir(t, featureDir, localFeatureDir)

	dc := map[string]any{
		"name":  "aileron-sandbox-feature-" + agent,
		"image": sandboxFeaturesBaseImage,
		"features": map[string]any{
			"./" + agent: map[string]any{},
		},
	}
	raw, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		t.Fatalf("marshal devcontainer.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "devcontainer.json"), raw, 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}

	image := fmt.Sprintf("aileron/sandbox-feature-%s:test", agent)
	args := []string{
		"build",
		"--workspace-folder", workspace,
		"--image-name", image,
	}
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, "devcontainer", args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		t.Fatalf("devcontainer build for %s failed: %v\nstdout:\n%s\nstderr:\n%s", agent, err, stdout.String(), stderr.String())
	}
	t.Logf("devcontainer build %s ->\n%s", agent, stdout.String())
	return image
}

// copyFeatureDir copies the Feature's files (devcontainer-feature.json,
// install.sh) from src into dst, preserving file modes so install.sh keeps
// its executable bit. The Feature directory is flat (no subdirectories), so a
// shallow copy suffices.
func copyFeatureDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir feature dst %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read feature dir %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		dstPath := filepath.Join(dst, e.Name())
		if err := os.WriteFile(dstPath, raw, info.Mode().Perm()); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
		// WriteFile's create mode is masked by umask, so re-apply the source
		// mode explicitly to preserve install.sh's executable bit.
		if err := os.Chmod(dstPath, info.Mode().Perm()); err != nil {
			t.Fatalf("chmod %s: %v", e.Name(), err)
		}
	}
}

// assertCommandOnPath runs the built image and asserts `command -v <cmd>`
// exits 0, proving the Feature put the agent CLI on PATH.
func assertCommandOnPath(ctx context.Context, t *testing.T, rt, image, cmd string) {
	t.Helper()
	args := []string{
		"run", "--rm",
		image,
		"/bin/sh", "-c", "command -v " + cmd,
	}
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, rt, args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		t.Fatalf("`command -v %s` failed in image %s: %v\nstdout: %q\nstderr: %q\nThe Feature must put %q on PATH for the running user.", cmd, image, err, stdout.String(), stderr.String(), cmd)
	}
	if path := strings.TrimSpace(stdout.String()); path == "" {
		t.Fatalf("`command -v %s` produced no PATH entry in image %s; the Feature did not install the CLI", cmd, image)
	} else {
		t.Logf("%s resolved to %s in image %s", cmd, path, image)
	}
}
