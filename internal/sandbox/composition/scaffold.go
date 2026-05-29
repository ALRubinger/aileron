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

# --- GitHub CLI ---
# RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && \
#     curl -fsSL https://github.com/cli/cli/releases/latest/download/gh_*_linux_amd64.tar.gz | \
#       tar xz -C /usr/local --strip-components=1

# --- Node.js (via fnm) ---
# RUN curl -fsSL https://fnm.vercel.app/install | bash && \
#     /root/.local/share/fnm/fnm install 20 && \
#     ln -s /root/.local/share/fnm/aliases/default/bin/node /usr/local/bin/node && \
#     ln -s /root/.local/share/fnm/aliases/default/bin/npm /usr/local/bin/npm

# --- Python (via mise) ---
# RUN curl https://mise.run | sh && \
#     /root/.local/bin/mise install python@3.11 && \
#     ln -s /root/.local/share/mise/installs/python/3.11/bin/python /usr/local/bin/python

# --- kubectl ---
# RUN curl -LO https://dl.k8s.io/release/v1.29.0/bin/linux/amd64/kubectl && \
#     install -m 0755 kubectl /usr/local/bin/

# --- Terraform ---
# RUN apt-get update && apt-get install -y --no-install-recommends gnupg lsb-release curl ca-certificates && \
#     curl -fsSL https://apt.releases.hashicorp.com/gpg | gpg --dearmor -o /usr/share/keyrings/hashicorp.gpg && \
#     echo "deb [signed-by=/usr/share/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" > /etc/apt/sources.list.d/hashicorp.list && \
#     apt-get update && apt-get install -y --no-install-recommends terraform
`, BaseImage(version))
}
