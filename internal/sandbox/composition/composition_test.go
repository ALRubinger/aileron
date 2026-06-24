package composition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverMissingDevcontainerUsesBaseImage(t *testing.T) {
	plan, err := Discover(t.TempDir(), "0.4.0", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierBase {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierBase)
	}
	if plan.Image != "ghcr.io/alrubinger/aileron-sandbox-base:latest" {
		t.Fatalf("Image = %q", plan.Image)
	}
}

func TestDiscoverMissingDevcontainerPublishableAgentResolvesPerAgentImage(t *testing.T) {
	// Codex is publishable (Apache-2.0): no .devcontainer resolves the build-free
	// published per-agent image.
	plan, err := Discover(t.TempDir(), "0.4.0", "codex")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierPublished {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierPublished)
	}
	if plan.Image != "ghcr.io/alrubinger/aileron-sandbox-codex:latest" {
		t.Fatalf("Image = %q, want %q", plan.Image, "ghcr.io/alrubinger/aileron-sandbox-codex:latest")
	}
	if plan.BaseImage != BaseImage("0.4.0") {
		t.Fatalf("BaseImage = %q, want %q", plan.BaseImage, BaseImage("0.4.0"))
	}
	if plan.SynthesizedDevcontainer != "" {
		t.Fatalf("SynthesizedDevcontainer should be empty for a published agent, got %q", plan.SynthesizedDevcontainer)
	}
}

func TestDiscoverMissingDevcontainerNonPublishableAgentResolvesLocalBuild(t *testing.T) {
	// Claude has a recipe but is NOT publishable (@anthropic-ai/claude-code is
	// all-rights-reserved, no redistribution grant, #1451). With no .devcontainer
	// it must resolve a LOCAL Feature build (TierDevcontainer with the agent
	// Feature and a synthesized devcontainer.json), never a published image pull.
	plan, err := Discover(t.TempDir(), "0.4.0", "claude")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	if plan.Image != LocalAgentImageTag("claude", "0.4.0") {
		t.Fatalf("Image = %q, want local tag %q", plan.Image, LocalAgentImageTag("claude", "0.4.0"))
	}
	// The plan must NOT name any aileron-sandbox-claude published image: that
	// would redistribute Claude Code, the exact violation #1451 fixes.
	if strings.Contains(plan.Image, "aileron-sandbox-claude") {
		t.Fatalf("Image = %q must not reference a published claude image", plan.Image)
	}
	want := FeatureReference("claude")
	if _, ok := plan.Features[want]; !ok {
		t.Fatalf("Features = %v, want it to contain the claude Feature %q", plan.Features, want)
	}
	if len(plan.Features) != 1 {
		t.Fatalf("Features = %v, want exactly the claude Feature", plan.Features)
	}
	if plan.SynthesizedDevcontainer == "" {
		t.Fatalf("SynthesizedDevcontainer must be set for a local agent build")
	}
	// The synthesized config must compose the base image with the claude Feature
	// and nothing else (no published claude image baked in).
	if !strings.Contains(plan.SynthesizedDevcontainer, BaseImage("0.4.0")) {
		t.Fatalf("SynthesizedDevcontainer %q must FROM the base image", plan.SynthesizedDevcontainer)
	}
	if !strings.Contains(plan.SynthesizedDevcontainer, want) {
		t.Fatalf("SynthesizedDevcontainer %q must reference the claude Feature %q", plan.SynthesizedDevcontainer, want)
	}
	if strings.Contains(plan.SynthesizedDevcontainer, "aileron-sandbox-claude") {
		t.Fatalf("SynthesizedDevcontainer %q must not reference a published claude image", plan.SynthesizedDevcontainer)
	}
	if plan.BaseImage != BaseImage("0.4.0") {
		t.Fatalf("BaseImage = %q, want %q", plan.BaseImage, BaseImage("0.4.0"))
	}
}

func TestDiscoverMissingDevcontainerLocalAgentInheritsBakedMCPBase(t *testing.T) {
	// Regression lock for #1457. The local Claude image is built FROM the
	// PUBLISHED sandbox-base, which bakes aileron-mcp at /usr/local/bin and
	// stamps the ai.aileron.mcp.version label (images/sandbox-base/Containerfile.published).
	// A child image built FROM that base inherits BOTH the baked binary and the
	// label, so the launcher's BakedMCPVersion (container.MCPVersionLabel) reads
	// non-empty and the local Claude path skips the host-mount of aileron-mcp
	// entirely (dodging the #1447 Windows-junction friction). That inheritance,
	// not a per-image COPY, is what satisfies the operator intent.
	//
	// The invariant this test guards: the synthesized local-build devcontainer's
	// `image` field is EXACTLY the published, labeled base repository. If a future
	// change repoints BaseImage at the unlabeled local Tier 0 base
	// (images/sandbox-base/Containerfile, which carries no label and bakes no
	// aileron-mcp) or drops the image field, the label is no longer inherited and
	// the local Claude path silently regresses to the host-mount. Asserting the
	// exact published-base reference (rather than a substring) catches that.
	plan, err := Discover(t.TempDir(), "0.4.0", "claude")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	var cfg struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal([]byte(plan.SynthesizedDevcontainer), &cfg); err != nil {
		t.Fatalf("parse synthesized devcontainer %q: %v", plan.SynthesizedDevcontainer, err)
	}
	// The base must be the PUBLISHED repository (it is the only sandbox-base
	// recipe that stamps ai.aileron.mcp.version and bakes aileron-mcp), pinned to
	// the resolved version tag. Equal, not Contains: a substring match would also
	// pass for an unlabeled local base whose name happened to contain this one.
	if cfg.Image != BaseImage("0.4.0") {
		t.Fatalf("synthesized image = %q, want the published labeled base %q", cfg.Image, BaseImage("0.4.0"))
	}
	if !strings.HasPrefix(cfg.Image, DefaultBaseImageRepository+":") {
		t.Fatalf("synthesized image = %q, want it to FROM the published base repository %q so the local Claude image inherits the baked aileron-mcp + ai.aileron.mcp.version label", cfg.Image, DefaultBaseImageRepository)
	}
}

func TestDiscoverMissingDevcontainerUnpublishedAgentFallsBackToBase(t *testing.T) {
	// goose has no recipe/published image, so it must keep the base behavior so
	// the downstream actionable "install the agent CLI or --sandbox=off" path
	// stays intact.
	plan, err := Discover(t.TempDir(), "0.4.0", "goose")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierBase {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierBase)
	}
	if plan.Image != BaseImage("0.4.0") {
		t.Fatalf("Image = %q, want %q", plan.Image, BaseImage("0.4.0"))
	}
}

func TestDiscoverMissingDevcontainerEmptyAgentStaysOnBase(t *testing.T) {
	// `sandbox plan`/`build` pass no agent; that path must keep the base tier.
	plan, err := Discover(t.TempDir(), "0.4.0", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierBase {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierBase)
	}
}

func TestDiscoverDevcontainerIgnoresAgentForTierResolution(t *testing.T) {
	// A present .devcontainer resolves Tier 1/Tier 2 regardless of the agent;
	// the agent param only affects the no-.devcontainer branch.
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "image": "ghcr.io/acme/custom:1",
  "customizations": {"aileron": {"image": "ghcr.io/acme/byo:1"}}
}`)
	plan, err := Discover(dir, "0.4.0", "claude")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierBYOImage {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierBYOImage)
	}
	if plan.Image != "ghcr.io/acme/byo:1" {
		t.Fatalf("Image = %q", plan.Image)
	}
}

func TestPublishedAgentImage(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"", "ghcr.io/alrubinger/aileron-sandbox-claude:edge"},
		{"dev", "ghcr.io/alrubinger/aileron-sandbox-claude:edge"},
		{"0.4.0", "ghcr.io/alrubinger/aileron-sandbox-claude:latest"},
	} {
		if got := PublishedAgentImage("claude", tc.version); got != tc.want {
			t.Fatalf("PublishedAgentImage(claude, %q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestPublishedAgentExists(t *testing.T) {
	// PublishedAgentExists is now publish-eligibility, not recipe-existence:
	// claude has a recipe but is NOT publishable (#1451), so it is false here.
	for agent, want := range map[string]bool{
		"claude": false,
		"codex":  true,
		"goose":  false,
		"":       false,
	} {
		if got := PublishedAgentExists(agent); got != want {
			t.Fatalf("PublishedAgentExists(%q) = %v, want %v", agent, got, want)
		}
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

	plan, err := Discover(dir, "dev", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	if plan.BaseImage != "ghcr.io/alrubinger/aileron-sandbox-base:edge" {
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

func TestDiscoverParsesCachePaths(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "customizations": {
    "aileron": {
      "cache_paths": [
        {"cli": "npm", "identity": "workspace-A", "container_path": "/home/agent/.npm"},
        {"cli": "pip", "identity": "workspace-A", "container_path": "/home/agent/.cache/pip"}
      ]
    }
  }
}`)

	plan, err := Discover(dir, "dev", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(plan.Aileron.CachePaths) != 2 {
		t.Fatalf("CachePaths = %+v, want 2 entries", plan.Aileron.CachePaths)
	}
	first := plan.Aileron.CachePaths[0]
	if first.CLI != "npm" || first.Identity != "workspace-A" || first.ContainerPath != "/home/agent/.npm" {
		t.Fatalf("first CachePath = %+v", first)
	}
	second := plan.Aileron.CachePaths[1]
	if second.CLI != "pip" || second.ContainerPath != "/home/agent/.cache/pip" {
		t.Fatalf("second CachePath = %+v", second)
	}
}

func TestDiscoverNoCachePathsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "customizations": {"aileron": {"approval_surface": "tui"}}
}`)
	plan, err := Discover(dir, "dev", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(plan.Aileron.CachePaths) != 0 {
		t.Fatalf("CachePaths = %+v, want none", plan.Aileron.CachePaths)
	}
}

func TestDiscoverDevcontainerRootDockerfileAndImage(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "dockerFile": "Dockerfile.custom"
}`)

	plan, err := Discover(dir, "0.4.0", "")
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

	plan, err := Discover(dir, "0.4.0", "")
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

	_, err := Discover(dir, "0.4.0", "")
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
	plan, err := Discover(dir, "0.4.0", "")
	if err != nil {
		t.Fatalf("Discover scaffolded devcontainer: %v", err)
	}
	if plan.Tier != TierDevcontainer {
		t.Fatalf("Tier = %s, want %s", plan.Tier, TierDevcontainer)
	}
	if plan.Image != "ghcr.io/alrubinger/aileron-sandbox-base:latest" {
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

	plan, err := Discover(dir, "0.4.0", "")
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

	plan, err := Discover(dir, "0.4.0", "")
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

	plan, err := Discover(dir, "0.4.0", "")
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

	_, err := Discover(dir, "0.4.0", "")
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
