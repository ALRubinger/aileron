//go:build integration_sandbox

// Sandbox Feature composition e2e test (#1083).
//
// Where sandbox_features_integration_test.go (#1082) drives @devcontainers/cli
// directly to prove the per-agent Feature install scripts work, this test
// proves the Aileron composition contract: a devcontainer.json that lists the
// Codex agent Feature plus a trivial second Feature, run THROUGH Aileron's own
// composition.Discover -> container.Builder.Build plumbing, produces an image
// whose declared tools both resolve on PATH.
//
// The build path under test routes through @devcontainers/cli because the plan
// carries `features` (raw `docker build` cannot apply Features); the Builder
// invokes it through the container.Runner seam via the pinned `npx
// @devcontainers/cli@<DevcontainerCLIVersion>` indirection.
//
// Per the umbrella's two-layer test posture this test is FAIL-FAST: there is no
// t.Skip. If Go, Node, npx/@devcontainers/cli, Docker, or the base image is
// absent, the test FAILS. CI provisions the toolchain (see the
// integration-sandbox-features job).
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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// TestSandboxFeaturesComposeViaAileron composes the Codex agent Feature plus a
// trivial second Feature through Aileron's Discover -> Build path and asserts
// both declared tools are on PATH in the built image.
func TestSandboxFeaturesComposeViaAileron(t *testing.T) {
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		// Fail-fast: CI provisions Docker; absence is a job failure, not a skip.
		t.Fatalf("no docker runtime on PATH (required, not skipped): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// The composed image FROMs a freshly built sandbox-base under a known tag so
	// the build does not depend on a registry-published base.
	buildSandboxFeaturesBaseImage(ctx, t, rt)

	featuresRoot := sandboxFeaturesContext(t)
	codexFeatureDir := filepath.Join(featuresRoot, "codex")
	if _, err := os.Stat(filepath.Join(codexFeatureDir, "devcontainer-feature.json")); err != nil {
		t.Fatalf("codex feature not found at %s: %v", codexFeatureDir, err)
	}

	workspace := t.TempDir()
	devDir := filepath.Join(workspace, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}

	// Copy the Codex Feature into .devcontainer/codex so it is referenced by the
	// only path form @devcontainers/cli accepts for a local Feature: a path
	// relative to .devcontainer.
	copyFeatureDir(t, codexFeatureDir, filepath.Join(devDir, "codex"))

	// A trivial second Feature, authored inline, that installs a unique probe
	// tool onto PATH. This proves composition of MORE than one Feature, and that
	// a customer's own tooling Feature composes alongside an agent Feature.
	const probeTool = "aileron-feature-probe"
	writeProbeFeature(t, filepath.Join(devDir, "probe"), probeTool)

	dc := map[string]any{
		"name":  "aileron-sandbox-features-composition",
		"image": sandboxFeaturesBaseImage,
		"features": map[string]any{
			"./codex": map[string]any{},
			"./probe": map[string]any{},
		},
	}
	raw, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		t.Fatalf("marshal devcontainer.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "devcontainer.json"), raw, 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}

	// Drive Aileron's own composition + build plumbing.
	plan, err := sandboxcomposition.Discover(workspace, "test")
	if err != nil {
		t.Fatalf("composition.Discover: %v", err)
	}
	if plan.Tier != sandboxcomposition.TierDevcontainer {
		t.Fatalf("plan.Tier = %s, want %s", plan.Tier, sandboxcomposition.TierDevcontainer)
	}
	if len(plan.Features) != 2 {
		t.Fatalf("plan.Features = %#v, want 2 (codex + probe)", plan.Features)
	}

	builder := sandboxcontainer.Builder{
		Runtime: rt,
		Stdout:  testLogWriter{t},
		Stderr:  testLogWriter{t},
	}
	result, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		WorkDir: workspace,
		Plan:    plan,
		Policy:  sandboxcontainer.BuildPolicyAlways,
	})
	if err != nil {
		t.Fatalf("Builder.Build via @devcontainers/cli: %v", err)
	}
	if !result.Built {
		t.Fatalf("result.Built = false, want true (features require a build)")
	}
	if result.Image == "" {
		t.Fatalf("result.Image is empty")
	}

	// The customer-tooling Feature's probe tool must resolve on PATH.
	assertCommandOnPath(ctx, t, rt, result.Image, probeTool)

	// Launchability: the composed image must satisfy the launch-time runtime
	// contract, not merely carry the agent CLI on PATH. Builder.Validate runs
	// the same probe the launcher uses — a writable /home/agent/workspace mount,
	// CWD there, and the agent command resolvable — so the test reflects that a
	// features-composed image is actually launchable, not just tool-on-PATH.
	// RequireMCPBinary is false here because aileron-mcp is bind-mounted at
	// launch (ADR-0024), not baked into the base image under test.
	validateWorkspace := t.TempDir()
	if err := builder.Validate(ctx, sandboxcontainer.ValidateOptions{
		Runtime: rt,
		Image:   result.Image,
		WorkDir: validateWorkspace,
		Command: []string{"codex"},
	}); err != nil {
		t.Fatalf("Builder.Validate (launchability of composed image): %v", err)
	}
}

// writeProbeFeature authors a minimal local devcontainer Feature whose
// install.sh drops an executable `tool` onto PATH. It exists to prove
// multi-Feature composition without depending on a published Feature ref.
func writeProbeFeature(t *testing.T, dir, tool string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir probe feature: %v", err)
	}
	manifest := map[string]any{
		"id":      "probe",
		"version": "1.0.0",
		"name":    "Aileron Feature Probe",
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal probe manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devcontainer-feature.json"), raw, 0o644); err != nil {
		t.Fatalf("write probe manifest: %v", err)
	}
	install := "#!/bin/sh\nset -e\n" +
		"printf '#!/bin/sh\\necho aileron-feature-probe\\n' > /usr/local/bin/" + tool + "\n" +
		"chmod +x /usr/local/bin/" + tool + "\n"
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte(install), 0o755); err != nil {
		t.Fatalf("write probe install.sh: %v", err)
	}
}
