//go:build integration_sandbox

// Composed-image credential-convention e2e test (#1857).
//
// Where the sibling composition tests (TestSandboxFeaturesComposeViaAileron /
// TestGHFeatureComposesViaAileron, #1083) prove declared tools resolve on PATH,
// this test closes the credential-convention coverage gap left by seam-level
// fakes: it composes a REAL image (Aileron sandbox base + the aws-cli Feature +
// the gh Feature) through Aileron's own composition.Discover ->
// container.Builder.Build path, reads the composed image's devcontainer.metadata
// OCI label through the exact production read path the launcher uses
// (container.ImageMetadataLabel, called at boot by
// cmd/aileron/skill_launch_proxy.go), and asserts it parses via
// composition.ConventionsFromMetadata into BOTH credential conventions:
// aws-cli's sigv4-resign and gh's bearer.
//
// This exercises the real contract (composed image -> devcontainer.metadata ->
// both conventions parse), not internals. If a Feature ever drops its
// credential block from the merged metadata, this test fails loudly — that is a
// real regression, so there is no t.Skip.
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
// or directly (from internal/, with AILERON_SANDBOX_BASE_CONTEXT +
// AILERON_SANDBOX_FEATURES_CONTEXT set as the task does):
//
//	go test -tags=integration_sandbox -run TestComposedImageCredentialMetadata ./launch/...
package launch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/credential/inject"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// TestComposedImageCredentialMetadata composes the aws-cli Feature plus the gh
// Feature through Aileron's Discover -> Build path, reads the composed image's
// devcontainer.metadata label via the production read path, and asserts both
// credential conventions (aws-cli's sigv4-resign and gh's bearer) parse out of
// the merged metadata with the expected placeholder env sets. The two
// credential-carrying Features are themselves the >=2-Feature composition, so
// no probe Feature is needed and len(plan.Features) == 2 is exact.
func TestComposedImageCredentialMetadata(t *testing.T) {
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
	awsFeatureDir := filepath.Join(featuresRoot, "aws-cli")
	if _, err := os.Stat(filepath.Join(awsFeatureDir, "devcontainer-feature.json")); err != nil {
		t.Fatalf("aws-cli feature not found at %s: %v", awsFeatureDir, err)
	}
	ghFeatureDir := filepath.Join(featuresRoot, "gh")
	if _, err := os.Stat(filepath.Join(ghFeatureDir, "devcontainer-feature.json")); err != nil {
		t.Fatalf("gh feature not found at %s: %v", ghFeatureDir, err)
	}

	workspace := t.TempDir()
	devDir := filepath.Join(workspace, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}

	// Copy both Features into .devcontainer/<name>: the only path form
	// @devcontainers/cli accepts for a local Feature is relative to .devcontainer.
	copyFeatureDir(t, awsFeatureDir, filepath.Join(devDir, "aws-cli"))
	copyFeatureDir(t, ghFeatureDir, filepath.Join(devDir, "gh"))

	dc := map[string]any{
		"name":  "aileron-credential-metadata-composition",
		"image": sandboxFeaturesBaseImage,
		"features": map[string]any{
			"./aws-cli": map[string]any{},
			"./gh":      map[string]any{},
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
	plan, err := sandboxcomposition.Discover(workspace, "test", "")
	if err != nil {
		t.Fatalf("composition.Discover: %v", err)
	}
	if plan.Tier != sandboxcomposition.TierDevcontainer {
		t.Fatalf("plan.Tier = %s, want %s", plan.Tier, sandboxcomposition.TierDevcontainer)
	}
	if len(plan.Features) != 2 {
		t.Fatalf("plan.Features = %#v, want 2 (aws-cli + gh)", plan.Features)
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
		// This test exercises Feature composition + metadata merging, not
		// toolchain provisioning; pin host-npx so it keeps its pre-#1530
		// behavior now that the default is managed. The managed path has its
		// own coverage in the Sandbox Managed integration matrix.
		ToolchainMode: sandboxcontainer.ToolchainModeHostNPX,
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

	// Read the merged devcontainer.metadata label through the EXACT production
	// read path the launcher uses at boot (skill_launch_proxy.go). An empty
	// label means the merged metadata never carried the credential blocks — a
	// real regression, not a skip.
	label := sandboxcontainer.ImageMetadataLabel(ctx, sandboxcontainer.DefaultRunner(), rt, result.Image)
	if strings.TrimSpace(label) == "" {
		t.Fatalf("composed image %s has an empty devcontainer.metadata label; expected merged aws-cli + gh credential blocks", result.Image)
	}

	// Parse the label into credential conventions via the contract the launcher
	// depends on.
	convs, err := sandboxcomposition.ConventionsFromMetadata([]byte(label))
	if err != nil {
		t.Fatalf("ConventionsFromMetadata(%q): %v", label, err)
	}
	if len(convs) != 2 {
		t.Fatalf("ConventionsFromMetadata returned %d conventions, want 2 (aws-cli sigv4-resign + gh bearer); label=%s", len(convs), label)
	}

	// Assert the two conventions by scheme, unordered: metadata array/merge
	// order is not part of the contract. Match each expected convention by its
	// inject.Scheme, then assert its placeholder env-key set.
	byScheme := map[inject.Scheme][]string{}
	for _, conv := range convs {
		if _, dup := byScheme[conv.Scheme]; dup {
			t.Fatalf("duplicate convention for scheme %q; want exactly one aws-cli (%s) and one gh (%s); label=%s",
				conv.Scheme, inject.SchemeSigV4Resign, inject.SchemeBearer, label)
		}
		envs := make([]string, 0, len(conv.Placeholders))
		for _, p := range conv.Placeholders {
			envs = append(envs, p.Env)
		}
		byScheme[conv.Scheme] = envs
	}

	assertEnvSet(t, byScheme, inject.SchemeSigV4Resign, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}, label)
	assertEnvSet(t, byScheme, inject.SchemeBearer, []string{"GH_TOKEN"}, label)
}

// assertEnvSet asserts that the conventions-by-scheme map carries the given
// scheme and that its placeholder env keys equal want as an unordered set.
func assertEnvSet(t *testing.T, byScheme map[inject.Scheme][]string, scheme inject.Scheme, want []string, label string) {
	t.Helper()
	got, ok := byScheme[scheme]
	if !ok {
		t.Fatalf("no credential convention with scheme %q found among %v; label=%s", scheme, keysOf(byScheme), label)
	}
	if !sameStringSet(got, want) {
		t.Fatalf("scheme %q placeholder env keys = %v, want %v (unordered); label=%s", scheme, got, want, label)
	}
}

// keysOf returns the schemes present in a conventions-by-scheme map, for error
// messages.
func keysOf(m map[inject.Scheme][]string) []inject.Scheme {
	out := make([]inject.Scheme, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sameStringSet reports whether a and b contain the same elements, ignoring
// order and duplicates.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	want := make(map[string]struct{}, len(b))
	for _, s := range b {
		want[s] = struct{}{}
	}
	if len(seen) != len(want) {
		return false
	}
	for s := range want {
		if _, ok := seen[s]; !ok {
			return false
		}
	}
	return true
}
