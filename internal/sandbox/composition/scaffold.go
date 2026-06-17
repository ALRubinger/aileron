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
