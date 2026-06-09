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
	if plan.Image != "aileron/sandbox-base:0.4.0" {
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
	if plan.BaseImage != "aileron/sandbox-base:latest" {
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

func TestInitWritesStarterDevcontainerAndDockerfile(t *testing.T) {
	dir := t.TempDir()
	result, err := Init(InitOptions{WorkDir: dir, Version: "0.4.0"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if result.DevcontainerPath != DefaultDevcontainerPath || result.DockerfilePath != DefaultDockerfilePath {
		t.Fatalf("result = %+v", result)
	}
	dev, err := os.ReadFile(filepath.Join(dir, DefaultDevcontainerPath))
	if err != nil {
		t.Fatalf("read devcontainer: %v", err)
	}
	if !strings.Contains(string(dev), `"customizations"`) || !strings.Contains(string(dev), `"aileron"`) {
		t.Fatalf("devcontainer missing Aileron customization:\n%s", string(dev))
	}
	dockerfile, err := os.ReadFile(filepath.Join(dir, DefaultDockerfilePath))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfile), "FROM aileron/sandbox-base:0.4.0") {
		t.Fatalf("Dockerfile =\n%s", string(dockerfile))
	}
	for _, line := range strings.Split(string(dockerfile), "\n") {
		trimmed := strings.TrimLeft(line, "# ")
		if strings.HasPrefix(trimmed, "RUN ") && strings.Contains(trimmed, "apt-get") {
			t.Fatalf("Dockerfile snippets must use apk against the Alpine base, not apt-get:\n%s", string(dockerfile))
		}
	}
	for _, snippet := range []string{"--- Claude Code ---", "--- GitHub CLI ---", "--- Node.js ---", "--- Python ---", "--- kubectl ---", "--- Terraform ---"} {
		if !strings.Contains(string(dockerfile), snippet) {
			t.Fatalf("Dockerfile missing snippet %q:\n%s", snippet, string(dockerfile))
		}
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

func TestInitForceOverwritesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{WorkDir: dir}); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	path := filepath.Join(dir, DefaultDockerfilePath)
	if err := os.WriteFile(path, []byte("custom"), 0o644); err != nil {
		t.Fatalf("write custom Dockerfile: %v", err)
	}
	if _, err := Init(InitOptions{WorkDir: dir, Force: true, Version: "0.4.0"}); err != nil {
		t.Fatalf("force Init: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if string(got) == "custom" {
		t.Fatal("Dockerfile was not overwritten")
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
