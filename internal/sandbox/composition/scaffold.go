package composition

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// InitOptions configures the sandbox scaffold operation.
type InitOptions struct {
	WorkDir string
	Version string
	Force   bool
}

// InitResult reports which files were written.
type InitResult struct {
	DevcontainerPath string
	DockerfilePath   string
}

// Init writes the starter devcontainer scaffold for sandbox composition.
func Init(opts InitOptions) (InitResult, error) {
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = "."
	}
	dir := filepath.Join(workDir, ".devcontainer")
	devcontainerPath := filepath.Join(workDir, DefaultDevcontainerPath)
	dockerfilePath := filepath.Join(workDir, DefaultDockerfilePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return InitResult{}, fmt.Errorf("create %s: %w", dir, err)
	}
	for _, path := range []string{devcontainerPath, dockerfilePath} {
		if _, err := os.Stat(path); err == nil && !opts.Force {
			return InitResult{}, fmt.Errorf("%s already exists (use --force to overwrite)", path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return InitResult{}, fmt.Errorf("stat %s: %w", path, err)
		}
	}
	if err := os.WriteFile(devcontainerPath, []byte(starterDevcontainer()), 0o644); err != nil {
		return InitResult{}, fmt.Errorf("write %s: %w", devcontainerPath, err)
	}
	if err := os.WriteFile(dockerfilePath, []byte(starterDockerfile(opts.Version)), 0o644); err != nil {
		return InitResult{}, fmt.Errorf("write %s: %w", dockerfilePath, err)
	}
	return InitResult{
		DevcontainerPath: DefaultDevcontainerPath,
		DockerfilePath:   DefaultDockerfilePath,
	}, nil
}

func starterDevcontainer() string {
	return `{
  "name": "Aileron sandbox",
  "build": {
    "dockerfile": "Dockerfile"
  },
  "customizations": {
    "aileron": {
      "mediation": "default",
      "approval_surface": "both"
    }
  }
}
`
}

func starterDockerfile(version string) string {
	return fmt.Sprintf(`FROM %s

# Uncomment snippets below to add tools to the Aileron sandbox.
# Keep rarely changing snippets earlier for better layer caching.
#
# Aileron's base image is Alpine-based and runs as the non-root "agent"
# user. Switch to root before installing packages, then switch back to
# agent before launch. All snippets use apk; do not use apt-get here.

# USER root

# --- Claude Code ---
# RUN apk add --no-cache git nodejs npm ripgrep && \
#     npm install -g @anthropic-ai/claude-code

# --- GitHub CLI ---
# RUN apk add --no-cache github-cli

# --- Node.js ---
# RUN apk add --no-cache nodejs npm

# --- Python ---
# RUN apk add --no-cache python3 py3-pip

# --- kubectl ---
# RUN apk add --no-cache kubectl

# --- Terraform ---
# RUN apk add --no-cache terraform

# USER agent
`, BaseImage(version))
}
