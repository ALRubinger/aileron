package composition

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultAgent is the agent whose Feature the scaffold demonstrates. It is no
// longer selectable from the CLI; `aileron sandbox init` always emits the
// Claude Feature as the worked example of the compose model, and customers
// swap or add Feature references by hand. Claude Code and Codex have published
// agent Features today (see docs/development/sandbox-agent-images.md).
const DefaultAgent = "claude"

// InitOptions configures the sandbox scaffold operation.
type InitOptions struct {
	WorkDir string
	Version string
	Force   bool
}

// InitResult reports which files were written.
type InitResult struct {
	DevcontainerPath string
}

// Init writes the starter devcontainer scaffold for sandbox composition. The
// scaffold composes the Aileron base image with a per-agent Feature, which is
// the Tier 1 customization model recorded in ADR-0017. It no longer writes a
// per-agent Dockerfile; the zero-build per-agent-image flow is owned by #965.
func Init(opts InitOptions) (InitResult, error) {
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = "."
	}
	dir := filepath.Join(workDir, ".devcontainer")
	devcontainerPath := filepath.Join(workDir, DefaultDevcontainerPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return InitResult{}, fmt.Errorf("create %s: %w", dir, err)
	}
	if _, err := os.Stat(devcontainerPath); err == nil && !opts.Force {
		return InitResult{}, fmt.Errorf("%s already exists (use --force to overwrite)", devcontainerPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return InitResult{}, fmt.Errorf("stat %s: %w", devcontainerPath, err)
	}
	if err := os.WriteFile(devcontainerPath, []byte(starterDevcontainer(opts.Version)), 0o644); err != nil {
		return InitResult{}, fmt.Errorf("write %s: %w", devcontainerPath, err)
	}
	return InitResult{DevcontainerPath: DefaultDevcontainerPath}, nil
}

// FeatureReference returns the canonical devcontainer Feature reference for an
// agent (e.g. "ghcr.io/alrubinger/aileron-features/claude:0"). It is the one
// place in Go that names the Feature registry path and tag, mirroring the
// BaseImage helper for the base image. The same reference identifies the
// Features authored under images/sandbox-features/<agent>/ and composed by the
// Tier 1 build path (#1083).
//
// The tag is ":0", the broadest in-house major tag. A "0.0.1" Feature manifest
// publishes the tag set "0.0.1", "0.0", "0", and "latest" through
// `devcontainer features publish`, so pinning ":0" lets 0.0.x patch bumps
// resolve without re-scaffolding. The scaffold stays on the major tag while the
// manifest holds the 0.0.x house version, keeping the two consistent without
// hard-coding a premature 1.0.0.
func FeatureReference(agent string) string {
	return DefaultFeatureRepository + "/" + agent + ":0"
}

// PublishedAgentImage returns the prebuilt per-agent sandbox image reference for
// an agent (e.g. "ghcr.io/alrubinger/aileron-sandbox-claude:latest"). It is the
// one place in Go that names the per-agent image registry path and tag,
// mirroring BaseImage for the base image. These images bake the agent CLI and
// aileron-mcp onto the base, so the build-free Tier 0 default pulls one instead
// of building the agent-less base (#1086, #1087).
//
// The tag follows the same normalization as BaseImage (see imageTag): a dev or
// empty version pulls the floating `edge` tag published off main on
// workflow_dispatch, and a release version pulls `latest`, which moves only on a
// v* tag build. The launcher resolves the floating pin at launch time because a
// version-pinned tag (<aileron-version>-<agent-cli-version>) requires the agent
// CLI version, which is not known here. Digest pinning and the freshness policy
// that consumes it are owned by #1088; this helper is the seam where that pinned
// reference plugs in.
func PublishedAgentImage(agent, version string) string {
	return DefaultPublishedImageRepository + "-" + agent + ":" + imageTag(version)
}

// PublishedAgentExists reports whether agent has a prebuilt per-agent image to
// pull for the build-free default. It reuses the agentRecipes set, which is the
// single source of truth for "this agent has a verified recipe and ships a
// published Feature/image" (see recipes.go). Discover and `sandbox check` share
// this predicate so both resolve the same image (the launch/check parity
// requirement). New agents that gain a recipe automatically gain auto-resolve.
func PublishedAgentExists(agent string) bool {
	_, ok := recipeForAgent(agent)
	return ok
}

// starterDevcontainer emits the Feature-composing devcontainer.json recorded in
// docs/development/sandbox-composition.md. It composes the Aileron base image
// with the default agent Feature and demonstrates the customer-tooling slot as
// a commented Feature so `Discover` parses exactly one active Feature. JSONC
// comments are valid here because Discover strips them before parsing.
func starterDevcontainer(version string) string {
	return fmt.Sprintf(`{
  "name": "Aileron sandbox",
  "image": %q,
  "features": {
    // The agent Feature installs the agent CLI onto the base image. Swap
    // "claude" for another published agent (e.g. "codex"), or list several.
    %q: {}
    // Add your own tooling as its own Feature alongside the agent Feature:
    // "ghcr.io/acme/internal-tools:1": {}
  },
  "customizations": {
    "aileron": {
      "mediation": "default",
      "approval_surface": "both"
    }
  }
}
`, BaseImage(version), FeatureReference(DefaultAgent))
}
