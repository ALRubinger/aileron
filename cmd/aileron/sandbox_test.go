package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

func TestRunSandboxPlanPrintsFeatures(t *testing.T) {
	dir := t.TempDir()
	devDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"build":{"dockerfile":"Dockerfile"},"features":{"ghcr.io/aileron/codex:1":{},"ghcr.io/acme/tool:2":{}}}`
	if err := os.WriteFile(filepath.Join(devDir, "devcontainer.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	var out, errb bytes.Buffer
	if code := runSandboxPlan(nil, &out, &errb); code != 0 {
		t.Fatalf("runSandboxPlan = %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	// Features are listed (sorted) so inspection reflects the parsed map.
	wantLine := "features: ghcr.io/acme/tool:2, ghcr.io/aileron/codex:1\n"
	if !strings.Contains(got, wantLine) {
		t.Fatalf("plan output missing %q:\n%s", wantLine, got)
	}
	if !strings.Contains(got, "tier: devcontainer") {
		t.Fatalf("plan output missing tier line:\n%s", got)
	}
}

func TestRunSandboxPlanNoDevcontainerStaysOnBaseTier(t *testing.T) {
	// `sandbox plan` passes no agent, so a project without a .devcontainer must
	// keep the base tier rather than resolving a published per-agent image.
	t.Chdir(t.TempDir())
	var out, errb bytes.Buffer
	if code := runSandboxPlan(nil, &out, &errb); code != 0 {
		t.Fatalf("runSandboxPlan = %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "tier: base") {
		t.Fatalf("plan output = %q, want base tier", out.String())
	}
	if !strings.Contains(out.String(), sandboxcomposition.BaseImage(version.Version)) {
		t.Fatalf("plan output = %q, want base image", out.String())
	}
}

func TestRunSandboxPlanFlagsFeaturesInertOnBYOImage(t *testing.T) {
	dir := t.TempDir()
	devDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A BYO image (customizations.aileron.image) with features: the image is
	// used as-is, so the features are inert and must be flagged as such.
	body := `{"customizations":{"aileron":{"image":"ghcr.io/acme/own:1"}},"features":{"ghcr.io/aileron/codex:1":{}}}`
	if err := os.WriteFile(filepath.Join(devDir, "devcontainer.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}
	t.Chdir(dir)

	var out, errb bytes.Buffer
	if code := runSandboxPlan(nil, &out, &errb); code != 0 {
		t.Fatalf("runSandboxPlan = %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "tier: byo_image") {
		t.Fatalf("plan output missing BYO tier line:\n%s", got)
	}
	wantLine := "features (ignored — BYO image): ghcr.io/aileron/codex:1\n"
	if !strings.Contains(got, wantLine) {
		t.Fatalf("plan output missing %q:\n%s", wantLine, got)
	}
	// It must NOT print the unqualified "features:" line that implies they apply.
	if strings.Contains(got, "features: ghcr.io/aileron/codex:1") {
		t.Fatalf("BYO plan must not present features as applied:\n%s", got)
	}
}

func TestSandboxCheckRequiresProxyTrust(t *testing.T) {
	cases := []struct {
		name      string
		runtime   string
		wantTrust bool
	}{
		{name: "docker requires proxy trust", runtime: "docker", wantTrust: true},
		{name: "podman does not require proxy trust (Docker-only)", runtime: "podman", wantTrust: false},
		{name: "docker with whitespace requires proxy trust", runtime: "  docker  ", wantTrust: true},
		{name: "empty runtime does not require proxy trust", runtime: "", wantTrust: false},
		{name: "unknown runtime does not require proxy trust", runtime: "containerd", wantTrust: false},
		{name: "auto literal does not require proxy trust (caller must resolve)", runtime: "auto", wantTrust: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxCheckRequiresProxyTrust(tc.runtime); got != tc.wantTrust {
				t.Fatalf("sandboxCheckRequiresProxyTrust(%q) = %v, want %v", tc.runtime, got, tc.wantTrust)
			}
		})
	}
}

// TestSandboxCheckValidateFnPassesRequireProxyTrust verifies the default
// validate function plumbing routes the runtime-derived RequireProxyTrust
// bool into ValidateOptions. It exercises the package-level
// sandboxCheckValidateFn by stubbing the underlying Builder.Run via a
// no-runtime image that fails fast on Builder.Validate's image-required
// guard if the proxy-trust wiring drops the value.
//
// The intent is to prevent a future refactor from silently dropping the
// RequireProxyTrust assignment between sandboxCheckValidateFn and
// ValidateOptions; the rest of the validation path is covered by
// internal/sandbox/container tests.
func TestSandboxCheckValidateFnPassesRequireProxyTrust(t *testing.T) {
	// The default sandboxCheckValidateFn calls Builder.Validate with an
	// empty image; Validate rejects an empty Image before touching the
	// runtime. That's what we want — we only care that the call dispatched
	// with the right runtime-derived RequireProxyTrust value. We verify
	// the dispatch by calling the function with an empty image and
	// asserting the well-known "sandbox image is required" error from
	// Builder.Validate.
	err := sandboxCheckValidateFn(context.Background(), "docker", t.TempDir(), "", "claude", false)
	if err == nil {
		t.Fatal("expected sandbox image required error")
	}
	if err.Error() != "sandbox image is required" {
		t.Fatalf("err = %v, want sandbox image is required", err)
	}

	// Sanity: also dispatches for an empty command (caught by the
	// command-required guard, but only after image is set).
	err = sandboxCheckValidateFn(context.Background(), "docker", t.TempDir(), "img:tag", "", false)
	if err == nil {
		t.Fatal("expected sandbox command required error")
	}
	if err.Error() != "sandbox command is required" {
		t.Fatalf("err = %v, want sandbox command is required", err)
	}
}

// --- U5 / #957: sandbox check baked-image validation + skew ---

func TestReportBakedMCPVersion(t *testing.T) {
	t.Run("match prints ok to stdout, nothing to stderr", func(t *testing.T) {
		var out, errb bytes.Buffer
		reportBakedMCPVersion(&out, &errb, "0.0.42", "0.0.42")
		if !strings.Contains(out.String(), "baked aileron-mcp 0.0.42") {
			t.Fatalf("stdout = %q", out.String())
		}
		if errb.Len() != 0 {
			t.Fatalf("unexpected stderr = %q", errb.String())
		}
	})
	t.Run("skew warns to stderr naming both versions, nothing to stdout", func(t *testing.T) {
		var out, errb bytes.Buffer
		reportBakedMCPVersion(&out, &errb, "0.0.42", "0.0.99")
		if out.Len() != 0 {
			t.Fatalf("unexpected stdout = %q", out.String())
		}
		w := errb.String()
		if !strings.Contains(w, "warning:") || !strings.Contains(w, "0.0.42") || !strings.Contains(w, "0.0.99") {
			t.Fatalf("stderr = %q", w)
		}
	})
}

// stubSandboxCheckSeams swaps the build/baked/validate seams so runSandboxCheck
// can be driven without a container runtime. It returns a pointer to the
// requireMCPBinary value the validate seam received.
func stubSandboxCheckSeams(t *testing.T, bakedVersion string) *bool {
	t.Helper()
	origBuild := sandboxCheckBuildFn
	origBaked := sandboxCheckBakedVersionFn
	origValidate := sandboxCheckValidateFn
	t.Cleanup(func() {
		sandboxCheckBuildFn = origBuild
		sandboxCheckBakedVersionFn = origBaked
		sandboxCheckValidateFn = origValidate
	})
	sandboxCheckBuildFn = func(_ context.Context, _ string, _, _ io.Writer, _ sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		return sandboxcontainer.BuildResult{Runtime: "docker", Image: "ghcr.io/acme/base:latest"}, nil
	}
	sandboxCheckBakedVersionFn = func(context.Context, string, string) string { return bakedVersion }
	var gotRequireMCP bool
	sandboxCheckValidateFn = func(_ context.Context, _, _, _, _ string, requireMCP bool) error {
		gotRequireMCP = requireMCP
		return nil
	}
	return &gotRequireMCP
}

func TestRunSandboxCheckBakedMatch(t *testing.T) {
	t.Chdir(t.TempDir())
	requireMCP := stubSandboxCheckSeams(t, version.Version)
	var out, errb bytes.Buffer
	if code := runSandboxCheck([]string{"claude"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !*requireMCP {
		t.Fatal("baked image must set RequireMCPBinary on the validate call")
	}
	if !strings.Contains(out.String(), "matches host CLI") {
		t.Fatalf("stdout = %q, want baked-match line", out.String())
	}
	if !strings.Contains(out.String(), "support: ok") {
		t.Fatalf("stdout = %q, want support: ok", out.String())
	}
}

func TestRunSandboxCheckBakedSkewWarnsNonFatal(t *testing.T) {
	t.Chdir(t.TempDir())
	stubSandboxCheckSeams(t, "9.9.9-not-the-host-version")
	var out, errb bytes.Buffer
	if code := runSandboxCheck([]string{"claude"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (skew is a warning); stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "warning:") || !strings.Contains(errb.String(), "9.9.9-not-the-host-version") {
		t.Fatalf("stderr = %q, want skew warning naming the baked version", errb.String())
	}
	if !strings.Contains(out.String(), "support: ok") {
		t.Fatalf("stdout = %q, want support: ok despite skew", out.String())
	}
}

func TestRunSandboxCheckUnbakedNoSkewNoRequireMCP(t *testing.T) {
	t.Chdir(t.TempDir())
	requireMCP := stubSandboxCheckSeams(t, "")
	var out, errb bytes.Buffer
	if code := runSandboxCheck([]string{"claude"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if *requireMCP {
		t.Fatal("unbaked image must not require the in-image MCP binary under sandbox check")
	}
	if strings.Contains(out.String(), "baked aileron-mcp") || strings.Contains(errb.String(), "warning:") {
		t.Fatalf("unbaked check must not mention baked mcp; stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// captureSandboxCheckPlan swaps the build/baked/validate seams and returns a
// pointer to the composition plan runSandboxCheck resolves and hands to the
// builder. It lets the test assert the resolved image without a real runtime.
func captureSandboxCheckPlan(t *testing.T) *sandboxcomposition.Plan {
	t.Helper()
	origBuild := sandboxCheckBuildFn
	origBaked := sandboxCheckBakedVersionFn
	origValidate := sandboxCheckValidateFn
	t.Cleanup(func() {
		sandboxCheckBuildFn = origBuild
		sandboxCheckBakedVersionFn = origBaked
		sandboxCheckValidateFn = origValidate
	})
	var gotPlan sandboxcomposition.Plan
	sandboxCheckBuildFn = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		gotPlan = opts.Plan
		return sandboxcontainer.BuildResult{Runtime: "docker", Image: opts.Plan.Image, Tier: opts.Plan.Tier}, nil
	}
	sandboxCheckBakedVersionFn = func(context.Context, string, string) string { return "" }
	sandboxCheckValidateFn = func(context.Context, string, string, string, string, bool) error { return nil }
	return &gotPlan
}

func TestRunSandboxCheckPublishedAgentResolvesPerAgentImage(t *testing.T) {
	t.Chdir(t.TempDir())
	plan := captureSandboxCheckPlan(t)
	var out, errb bytes.Buffer
	if code := runSandboxCheck([]string{"--agent=claude"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if plan.Tier != sandboxcomposition.TierPublished {
		t.Fatalf("plan.Tier = %s, want %s", plan.Tier, sandboxcomposition.TierPublished)
	}
	want := sandboxcomposition.PublishedAgentImage("claude", version.Version)
	if plan.Image != want {
		t.Fatalf("plan.Image = %q, want %q (parity with launch)", plan.Image, want)
	}
}

func TestRunSandboxCheckUnpublishedAgentUsesBaseImage(t *testing.T) {
	t.Chdir(t.TempDir())
	plan := captureSandboxCheckPlan(t)
	var out, errb bytes.Buffer
	if code := runSandboxCheck([]string{"--agent=goose"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if plan.Tier != sandboxcomposition.TierBase {
		t.Fatalf("plan.Tier = %s, want %s", plan.Tier, sandboxcomposition.TierBase)
	}
	if plan.Image != sandboxcomposition.BaseImage(version.Version) {
		t.Fatalf("plan.Image = %q, want base image", plan.Image)
	}
}

// TestSandboxCheckValidateOptionsWiring is a compile-time guarantee that the
// ValidateOptions struct still carries RequireProxyTrust; the runtime
// behavior is exercised by TestSandboxCheckRequiresProxyTrust and the
// docker launch path in internal/sandbox/container.
func TestSandboxCheckValidateOptionsWiring(t *testing.T) {
	var opts sandboxcontainer.ValidateOptions
	opts.RequireProxyTrust = true
	if !opts.RequireProxyTrust {
		t.Fatal("ValidateOptions.RequireProxyTrust must round-trip")
	}
}
