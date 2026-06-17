package composition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverMissingDevcontainerUsesBaseImage(t *testing.T) {
	plan, err := Discover(t.TempDir(), "0.4.0")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierBase {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierBase)
	}
	if plan.Image != "ghcr.io/alrubinger/aileron-sandbox-base:0.4.0" {
		t.Fatalf("Image = %q", plan.Image)
	}
}

func TestDiscoverDevcontainerBuildPlan(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  // JSONC comments are valid in devcontainer.json.
  "build": {
    "dockerfile": "Dockerfile",
    "args": {"NODE_VERSION": "20"}
  },
  "mounts": [
    "source=${localEnv:HOME}/.gitconfig,target=/home/agent/.gitconfig,type=bind,readonly",
    {"source": "/tmp/cache", "target": "/cache", "readonly": true}
  ],
  "customizations": {
    "aileron": {
      "mediation": "default",
      "approval_surface": "tui"
    }
  }
}`)

	plan, err := Discover(dir, "dev")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	if plan.BaseImage != "ghcr.io/alrubinger/aileron-sandbox-base:latest" {
		t.Fatalf("BaseImage = %q", plan.BaseImage)
	}
	if plan.DockerfilePath != "Dockerfile" {
		t.Fatalf("DockerfilePath = %q", plan.DockerfilePath)
	}
	if plan.BuildArgs["NODE_VERSION"] != "20" {
		t.Fatalf("BuildArgs = %+v", plan.BuildArgs)
	}
	if len(plan.Mounts) != 2 || plan.Mounts[1].Target != "/cache" || !plan.Mounts[1].ReadOnly {
		t.Fatalf("Mounts = %+v", plan.Mounts)
	}
	if plan.Aileron.ApprovalSurface != "tui" {
		t.Fatalf("Aileron = %+v", plan.Aileron)
	}
}

func TestDiscoverDevcontainerRootDockerfileAndImage(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "dockerFile": "Dockerfile.custom"
}`)

	plan, err := Discover(dir, "0.4.0")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	if plan.Image != "mcr.microsoft.com/devcontainers/base:ubuntu" {
		t.Fatalf("Image = %q", plan.Image)
	}
	if plan.DockerfilePath != "Dockerfile.custom" {
		t.Fatalf("DockerfilePath = %q", plan.DockerfilePath)
	}
}

func TestDiscoverAileronImageIsBYOImage(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "customizations": {
    "aileron": {
      "image": "ghcr.io/acme/agent:2026-05-29",
      "approval_surface": "both"
    }
  }
}`)

	plan, err := Discover(dir, "0.4.0")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierBYOImage {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierBYOImage)
	}
	if plan.Image != "ghcr.io/acme/agent:2026-05-29" {
		t.Fatalf("Image = %q", plan.Image)
	}
	if plan.DockerfilePath != "" {
		t.Fatalf("DockerfilePath = %q, want empty", plan.DockerfilePath)
	}
}

func TestDiscoverInvalidMountReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{"mounts":[42]}`)

	_, err := Discover(dir, "0.4.0")
	if err == nil {
		t.Fatal("expected mount parse error")
	}
	if !strings.Contains(err.Error(), "parse devcontainer mount") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseDevcontainerRejectsUnknownApprovalSurface(t *testing.T) {
	_, err := parseDevcontainer([]byte(`{"customizations":{"aileron":{"approval_surface":"sms"}}}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "approval_surface") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseDevcontainerAllowsCommentsOutsideStrings(t *testing.T) {
	cfg, err := parseDevcontainer([]byte(`{
  "image": "https://example.com/not-a-comment",
  /* keep line numbers stable */
  "customizations": {
    "aileron": {
      "approval_surface": "webapp"
    }
  }
}`))
	if err != nil {
		t.Fatalf("parseDevcontainer: %v", err)
	}
	if cfg.Image != "https://example.com/not-a-comment" {
		t.Fatalf("Image = %q", cfg.Image)
	}
}

func TestParseDevcontainerRejectsUnterminatedBlockComment(t *testing.T) {
	_, err := parseDevcontainer([]byte(`{"image":"x" /* unterminated`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unterminated block comment") {
		t.Fatalf("error = %v", err)
	}
}

func TestInitWritesFeatureComposingDevcontainer(t *testing.T) {
	dir := t.TempDir()
	result, err := Init(InitOptions{WorkDir: dir, Version: "0.4.0"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if result.DevcontainerPath != DefaultDevcontainerPath {
		t.Fatalf("DevcontainerPath = %q, want %q", result.DevcontainerPath, DefaultDevcontainerPath)
	}
	// init no longer writes a per-agent Dockerfile.
	if _, err := os.Stat(filepath.Join(dir, DefaultDockerfilePath)); !os.IsNotExist(err) {
		t.Fatalf("init should not write a Dockerfile: stat err = %v", err)
	}

	// Assert on the produced config shape via Discover (the contract), not on
	// the raw scaffold text.
	plan, err := Discover(dir, "0.4.0")
	if err != nil {
		t.Fatalf("Discover scaffolded devcontainer: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	if plan.Image != "ghcr.io/alrubinger/aileron-sandbox-base:0.4.0" {
		t.Fatalf("Image = %q, want the base image", plan.Image)
	}
	// Exactly one active Feature: the default agent Feature. The customer
	// tooling slot is commented out, so Discover must not see it.
	if len(plan.Features) != 1 {
		t.Fatalf("Features = %#v, want exactly one active feature", plan.Features)
	}
	if _, ok := plan.Features[FeatureReference("claude")]; !ok {
		t.Fatalf("Features missing %q: %#v", FeatureReference("claude"), plan.Features)
	}
	if plan.Aileron.Mediation != "default" {
		t.Fatalf("Aileron.Mediation = %q, want default", plan.Aileron.Mediation)
	}
	if plan.Aileron.ApprovalSurface != "both" {
		t.Fatalf("Aileron.ApprovalSurface = %q, want both", plan.Aileron.ApprovalSurface)
	}
}

func TestFeatureReferenceNamesPublishedFeature(t *testing.T) {
	if got, want := FeatureReference("claude"), DefaultFeatureRepository+"/claude:0"; got != want {
		t.Fatalf("FeatureReference = %q, want %q", got, want)
	}
}

func TestInitDoesNotOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{WorkDir: dir}); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	_, err := Init(InitOptions{WorkDir: dir})
	if err == nil {
		t.Fatal("expected overwrite error")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %v", err)
	}
}

func TestInitForceOverwritesExistingDevcontainer(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{WorkDir: dir}); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	path := filepath.Join(dir, DefaultDevcontainerPath)
	if err := os.WriteFile(path, []byte("custom"), 0o644); err != nil {
		t.Fatalf("write custom devcontainer: %v", err)
	}
	if _, err := Init(InitOptions{WorkDir: dir, Force: true, Version: "0.4.0"}); err != nil {
		t.Fatalf("force Init: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read devcontainer: %v", err)
	}
	if string(got) == "custom" {
		t.Fatal("devcontainer was not overwritten")
	}
}

func TestDiscoverParsesFeaturesIntoPlan(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "build": {"dockerfile": "Dockerfile"},
  "features": {
    "ghcr.io/aileron/codex:1": {},
    "ghcr.io/acme/tool:2": {"version": "latest"}
  },
  "customizations": {"aileron": {"approval_surface": "both"}}
}`)

	plan, err := Discover(dir, "0.4.0")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	if len(plan.Features) != 2 {
		t.Fatalf("Features = %#v, want 2 keys", plan.Features)
	}
	if got := strings.TrimSpace(string(plan.Features["ghcr.io/aileron/codex:1"])); got != "{}" {
		t.Fatalf("codex options = %q, want %q", got, "{}")
	}
	if got := strings.TrimSpace(string(plan.Features["ghcr.io/acme/tool:2"])); got != `{"version": "latest"}` {
		t.Fatalf("tool options = %q (raw payload must be preserved verbatim)", got)
	}
	// Features coexist with the Dockerfile and the approval surface.
	if plan.DockerfilePath != "Dockerfile" {
		t.Fatalf("DockerfilePath = %q", plan.DockerfilePath)
	}
	if plan.Aileron.ApprovalSurface != "both" {
		t.Fatalf("ApprovalSurface = %q", plan.Aileron.ApprovalSurface)
	}
}

func TestDiscoverWithoutFeaturesLeavesNilMap(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{"build": {"dockerfile": "Dockerfile"}}`)

	plan, err := Discover(dir, "0.4.0")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Features != nil {
		t.Fatalf("Features = %#v, want nil when no features block is present", plan.Features)
	}
}

func TestDiscoverCarriesFeaturesOnBYOImagePlan(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "features": {"ghcr.io/acme/tool:1": {}},
  "customizations": {"aileron": {"image": "ghcr.io/acme/agent:2026"}}
}`)

	plan, err := Discover(dir, "0.4.0")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierBYOImage {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierBYOImage)
	}
	// Features are carried for plan inspection even though they are inert on a
	// BYO image (no build occurs).
	if len(plan.Features) != 1 {
		t.Fatalf("Features = %#v, want 1 key carried on BYO plan", plan.Features)
	}
}

func TestDiscoverRejectsNonObjectFeatures(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{"features": ["ghcr.io/acme/tool:1"]}`)

	_, err := Discover(dir, "0.4.0")
	if err == nil {
		t.Fatal("expected parse error for non-object features")
	}
	if !strings.Contains(err.Error(), "features") {
		t.Fatalf("error = %v, want mention of features", err)
	}
}

func writeDevcontainer(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, DefaultDevcontainerPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
