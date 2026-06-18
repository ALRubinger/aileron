package composition

import (
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

func TestDiscoverMissingDevcontainerPublishedAgentResolvesPerAgentImage(t *testing.T) {
	for _, tc := range []struct {
		agent     string
		wantImage string
	}{
		{"claude", "ghcr.io/alrubinger/aileron-sandbox-claude:latest"},
		{"codex", "ghcr.io/alrubinger/aileron-sandbox-codex:latest"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			plan, err := Discover(t.TempDir(), "0.4.0", tc.agent)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if plan.Tier != TierPublished {
				t.Fatalf("Tier = %s, want %s", plan.Tier, TierPublished)
			}
			if plan.Image != tc.wantImage {
				t.Fatalf("Image = %q, want %q", plan.Image, tc.wantImage)
			}
			if plan.BaseImage != BaseImage("0.4.0") {
				t.Fatalf("BaseImage = %q, want %q", plan.BaseImage, BaseImage("0.4.0"))
			}
		})
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
	for agent, want := range map[string]bool{
		"claude": true,
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

func TestDiscoverParsesCachesIntoPlan(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "customizations": {
    "aileron": {
      "caches": [
        {"name": "printingpress", "path": "/home/agent/.local/share/printingpress"},
        {"name": "gh", "path": "/home/agent/.config/gh"}
      ]
    }
  }
}`)
	plan, err := Discover(dir, "0.4.0", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(plan.Aileron.Caches) != 2 {
		t.Fatalf("Caches = %+v, want 2", plan.Aileron.Caches)
	}
	if plan.Aileron.Caches[0].Name != "printingpress" || plan.Aileron.Caches[0].Path != "/home/agent/.local/share/printingpress" {
		t.Fatalf("Caches[0] = %+v", plan.Aileron.Caches[0])
	}
	if plan.Aileron.Caches[1].Name != "gh" || plan.Aileron.Caches[1].Path != "/home/agent/.config/gh" {
		t.Fatalf("Caches[1] = %+v", plan.Aileron.Caches[1])
	}
}

func TestParseDevcontainerRejectsInvalidCaches(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "missing name",
			json: `{"customizations":{"aileron":{"caches":[{"path":"/data"}]}}}`,
			want: "name is required",
		},
		{
			name: "missing path",
			json: `{"customizations":{"aileron":{"caches":[{"name":"x"}]}}}`,
			want: "path is required",
		},
		{
			name: "relative path",
			json: `{"customizations":{"aileron":{"caches":[{"name":"x","path":"data"}]}}}`,
			want: "must be absolute",
		},
		{
			name: "duplicate name",
			json: `{"customizations":{"aileron":{"caches":[{"name":"x","path":"/a"},{"name":"x","path":"/b"}]}}}`,
			want: "duplicate cache name",
		},
		{
			name: "duplicate path",
			json: `{"customizations":{"aileron":{"caches":[{"name":"x","path":"/a"},{"name":"y","path":"/a"}]}}}`,
			want: "duplicate cache path",
		},
		{
			name: "invalid name char",
			json: `{"customizations":{"aileron":{"caches":[{"name":"bad/name","path":"/a"}]}}}`,
			want: "must contain only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDevcontainer([]byte(tc.json))
			if err == nil {
				t.Fatalf("parseDevcontainer: expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateCachesAcceptsUnixContainerPath(t *testing.T) {
	// Cache paths are in-container Unix paths. They must validate as absolute
	// on every host OS, so validation uses the path package, not filepath
	// (filepath.IsAbs("/x") is false on Windows). Regression for the
	// cross-platform finding.
	_, err := parseDevcontainer([]byte(`{"customizations":{"aileron":{"caches":[{"name":"pp","path":"/home/agent/.local/share/pp"}]}}}`))
	if err != nil {
		t.Fatalf("parseDevcontainer rejected a valid Unix container path: %v", err)
	}
}

func TestCacheVolumeNameIsDeterministicAndWorkspaceKeyed(t *testing.T) {
	a := CacheVolumeName("claude", "/home/u/projA", "printingpress")
	again := CacheVolumeName("claude", "/home/u/projA", "printingpress")
	if a != again {
		t.Fatalf("CacheVolumeName not deterministic: %q vs %q", a, again)
	}
	if !strings.HasPrefix(a, CacheVolumeNamePrefix+"-claude-printingpress-") {
		t.Fatalf("CacheVolumeName = %q, want prefix %s-claude-printingpress-", a, CacheVolumeNamePrefix)
	}
	// Different workspace -> different volume (re-auth to a different
	// workspace gets a fresh store).
	if b := CacheVolumeName("claude", "/home/u/projB", "printingpress"); a == b {
		t.Fatalf("expected distinct volumes for distinct workspaces, both = %q", a)
	}
	// Different agent -> different volume.
	if b := CacheVolumeName("codex", "/home/u/projA", "printingpress"); a == b {
		t.Fatalf("expected distinct volumes for distinct agents, both = %q", a)
	}
	// Different cache name -> different volume.
	if b := CacheVolumeName("claude", "/home/u/projA", "gh"); a == b {
		t.Fatalf("expected distinct volumes for distinct cache names, both = %q", a)
	}
}

func TestResolveCaches(t *testing.T) {
	plan := Plan{Aileron: AileronCustomization{Caches: []Cache{
		{Name: "printingpress", Path: "/data/pp"},
		{Name: "gh", Path: "/data/gh"},
	}}}
	resolved, err := plan.ResolveCaches("claude", "/ws")
	if err != nil {
		t.Fatalf("ResolveCaches: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved = %+v, want 2", resolved)
	}
	if resolved[0].Cache.Path != "/data/pp" || resolved[0].VolumeName != CacheVolumeName("claude", "/ws", "printingpress") {
		t.Fatalf("resolved[0] = %+v", resolved[0])
	}
}

func TestResolveCachesEmptyReturnsNil(t *testing.T) {
	resolved, err := Plan{}.ResolveCaches("claude", "/ws")
	if err != nil {
		t.Fatalf("ResolveCaches: %v", err)
	}
	if resolved != nil {
		t.Fatalf("resolved = %+v, want nil", resolved)
	}
}

func TestResolveCachesRequiresAgent(t *testing.T) {
	plan := Plan{Aileron: AileronCustomization{Caches: []Cache{{Name: "x", Path: "/a"}}}}
	_, err := plan.ResolveCaches("", "/ws")
	if err == nil || !strings.Contains(err.Error(), "agent is required") {
		t.Fatalf("err = %v, want agent-required error", err)
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
