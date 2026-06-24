//go:build integration_sandbox

// Managed-toolchain sandbox Feature build e2e test (#1530).
//
// Where sandbox_features_composition_integration_test.go drives the same
// composition.Discover -> container.Builder.Build path through the host-npx
// toolchain (the build path execs `npx @devcontainers/cli`), this test proves the
// MANAGED toolchain: the Builder is wired with the real
// sandboxtoolchain.Provisioner{}, so the Features build resolves Node +
// @devcontainers/cli through the Aileron-managed (fetched + verified + cached)
// toolchain rather than the host's npx. Docker is the only host prerequisite for
// the build itself; the managed Node fetch needs network/registry access.
//
// This is the acceptance gate for the #1530 flip (managed is now the default
// toolchain). It composes the Claude Feature (the easy npm-install consumer) on a
// locally-built base, then asserts the build succeeds and the `claude` CLI is on
// PATH.
//
// Per the umbrella's two-layer test posture this test is FAIL-FAST on the
// required Linux leg: there is no t.Skip. If Docker, Node, the CLI, or the base
// image is absent, the test FAILS. The managed Node fetch's unsupported-target
// classification (the only legitimately "can't run here" outcome) is asserted as
// a clear, actionable message at the unit level in
// internal/sandbox/toolchain/toolchain_test.go, so it runs without Docker. The
// non-blocking macOS/Windows CI legs surface their result via continue-on-error,
// not t.Skip.
//
// Run with:
//
//	task test:integration:sandbox-managed
//
// or directly:
//
//	go test -tags=integration_sandbox -run TestSandboxFeaturesManaged ./launch/...
package launch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	sandboxtoolchain "github.com/ALRubinger/aileron/internal/sandbox/toolchain"
)

// TestSandboxFeaturesManagedToolchainBuildsClaude composes the Claude agent
// Feature through Aileron's Discover -> Build path with the MANAGED toolchain
// (real provisioner, no escape hatch) and asserts the `claude` CLI resolves on
// PATH in the built image.
func TestSandboxFeaturesManagedToolchainBuildsClaude(t *testing.T) {
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		// Fail-fast: CI provisions Docker; absence is a job failure, not a skip.
		t.Fatalf("no docker runtime on PATH (required, not skipped): %v", err)
	}

	// Generous timeout: the managed toolchain fetches + verifies Node and installs
	// the pinned @devcontainers/cli before the build, on top of the base image
	// build and the Feature install.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// FROM a freshly built sandbox-base under a known tag so the build does not
	// depend on a registry-published base.
	buildSandboxFeaturesBaseImage(ctx, t, rt)

	featuresRoot := sandboxFeaturesContext(t)
	claudeFeatureDir := filepath.Join(featuresRoot, "claude")
	if _, err := os.Stat(filepath.Join(claudeFeatureDir, "devcontainer-feature.json")); err != nil {
		t.Fatalf("claude feature not found at %s: %v", claudeFeatureDir, err)
	}

	workspace := t.TempDir()
	devDir := filepath.Join(workspace, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}

	// Copy the Claude Feature into .devcontainer/claude (the only path form
	// @devcontainers/cli accepts for a local Feature is relative to .devcontainer).
	copyFeatureDir(t, claudeFeatureDir, filepath.Join(devDir, "claude"))

	dc := map[string]any{
		"name":  "aileron-sandbox-features-managed-claude",
		"image": sandboxFeaturesBaseImage,
		"features": map[string]any{
			"./claude": map[string]any{},
		},
	}
	raw, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		t.Fatalf("marshal devcontainer.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "devcontainer.json"), raw, 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}

	plan, err := sandboxcomposition.Discover(workspace, "test", "")
	if err != nil {
		t.Fatalf("composition.Discover: %v", err)
	}
	if plan.Tier != sandboxcomposition.TierDevcontainer {
		t.Fatalf("plan.Tier = %s, want %s", plan.Tier, sandboxcomposition.TierDevcontainer)
	}
	if len(plan.Features) != 1 {
		t.Fatalf("plan.Features = %#v, want 1 (claude)", plan.Features)
	}

	// Wire the REAL managed-toolchain provisioner: zero-value Options take every
	// production default (pinned Node version, pinned CLI, host GOOS/GOARCH, the
	// embedded Node release keyring), with a CacheRoot under the test's temp dir
	// so the fetch+verify+cache happens against an isolated, disposable cache.
	builder := sandboxcontainer.Builder{
		Runtime: rt,
		Stdout:  testLogWriter{t},
		Stderr:  testLogWriter{t},
		Provisioner: sandboxtoolchain.Provisioner{Options: sandboxtoolchain.Options{
			CacheRoot: filepath.Join(t.TempDir(), "toolchain"),
		}},
	}
	result, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		WorkDir: workspace,
		Plan:    plan,
		Policy:  sandboxcontainer.BuildPolicyAlways,
		// Explicit managed selection (also the default after #1530); pinning it
		// here keeps the test's intent legible regardless of the default.
		ToolchainMode: sandboxcontainer.ToolchainModeManaged,
	})
	if err != nil {
		t.Fatalf("Builder.Build via managed toolchain: %v", err)
	}
	if !result.Built {
		t.Fatalf("result.Built = false, want true (features require a build)")
	}
	if result.Image == "" {
		t.Fatalf("result.Image is empty")
	}

	// The Claude CLI installed by the Feature must resolve on PATH in the image
	// built through the managed toolchain.
	assertCommandOnPath(ctx, t, rt, result.Image, "claude")
}

// TestSandboxFeaturesManagedDefaultBuildsClaude proves the #1530 flip end to end:
// with NO ToolchainMode set, the Builder takes the managed branch by default and
// invokes the wired provisioner, producing an image with `claude` on PATH. This
// is the same image contract as the explicit-managed test above, asserted through
// the default selection so the flip's user-facing behavior is covered.
func TestSandboxFeaturesManagedDefaultBuildsClaude(t *testing.T) {
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		t.Fatalf("no docker runtime on PATH (required, not skipped): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	buildSandboxFeaturesBaseImage(ctx, t, rt)

	featuresRoot := sandboxFeaturesContext(t)
	claudeFeatureDir := filepath.Join(featuresRoot, "claude")
	if _, err := os.Stat(filepath.Join(claudeFeatureDir, "devcontainer-feature.json")); err != nil {
		t.Fatalf("claude feature not found at %s: %v", claudeFeatureDir, err)
	}

	workspace := t.TempDir()
	devDir := filepath.Join(workspace, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}
	copyFeatureDir(t, claudeFeatureDir, filepath.Join(devDir, "claude"))

	dc := map[string]any{
		"name":  "aileron-sandbox-features-managed-default-claude",
		"image": sandboxFeaturesBaseImage,
		"features": map[string]any{
			"./claude": map[string]any{},
		},
	}
	raw, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		t.Fatalf("marshal devcontainer.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "devcontainer.json"), raw, 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}

	plan, err := sandboxcomposition.Discover(workspace, "test", "")
	if err != nil {
		t.Fatalf("composition.Discover: %v", err)
	}
	if plan.Tier != sandboxcomposition.TierDevcontainer {
		t.Fatalf("plan.Tier = %s, want %s", plan.Tier, sandboxcomposition.TierDevcontainer)
	}
	if len(plan.Features) != 1 {
		t.Fatalf("plan.Features = %#v, want 1 (claude)", plan.Features)
	}

	builder := sandboxcontainer.Builder{
		Runtime: rt,
		Stdout:  testLogWriter{t},
		Stderr:  testLogWriter{t},
		Provisioner: sandboxtoolchain.Provisioner{Options: sandboxtoolchain.Options{
			CacheRoot: filepath.Join(t.TempDir(), "toolchain"),
		}},
	}
	// No ToolchainMode set: the #1530 default must resolve to managed and invoke
	// the provisioner.
	result, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		WorkDir: workspace,
		Plan:    plan,
		Policy:  sandboxcontainer.BuildPolicyAlways,
	})
	if err != nil {
		t.Fatalf("Builder.Build via managed default: %v", err)
	}
	if !result.Built {
		t.Fatalf("result.Built = false, want true (features require a build)")
	}
	if result.Image == "" {
		t.Fatalf("result.Image is empty")
	}
	assertCommandOnPath(ctx, t, rt, result.Image, "claude")
}
