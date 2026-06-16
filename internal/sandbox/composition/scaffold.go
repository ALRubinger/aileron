package composition

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultAgent is the agent the scaffold targets when --agent is omitted.
// Claude Code and Codex have documented Tier 1 install recipes today (see
// docs/development/sandbox-agent-images.md); other agents emit a
// structurally correct Dockerfile with a TODO install stub.
const DefaultAgent = "claude"

// InitOptions configures the sandbox scaffold operation.
type InitOptions struct {
	WorkDir string
	Version string
	Force   bool
	Agent   string
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
	agent := opts.Agent
	if agent == "" {
		agent = DefaultAgent
	}
	if err := os.WriteFile(dockerfilePath, []byte(starterDockerfile(opts.Version, agent)), 0o644); err != nil {
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

func starterDockerfile(version, agent string) string {
	return fmt.Sprintf(`FROM %s

# Aileron's base image is Alpine-based and runs as the non-root "agent"
# user. Switch to root to install tools, then switch back to agent before
# launch. All install snippets use apk; do not use apt-get here.

USER root

%s

# Uncomment additional tool snippets below as needed. Keep rarely
# changing snippets earlier for better layer caching.
#
# # --- GitHub CLI ---
# RUN apk add --no-cache github-cli
#
# # --- Node.js ---
# RUN apk add --no-cache nodejs npm
#
# # --- Python ---
# RUN apk add --no-cache python3 py3-pip
#
# # --- kubectl ---
# RUN apk add --no-cache kubectl
#
# # --- Terraform ---
# RUN apk add --no-cache terraform

USER agent
`, BaseImage(version), agentInstallSnippet(agent))
}

// agentInstallSnippet returns the install recipe for agent. Agents with
// a documented Tier 1 recipe are pre-filled and uncommented; agents
// without one get a TODO stub that still produces a buildable Dockerfile
// once the user fills it in.
func agentInstallSnippet(agent string) string {
	switch agent {
	case "claude":
		return `# --- Claude Code ---
RUN apk add --no-cache git nodejs npm ripgrep && \
    npm install -g @anthropic-ai/claude-code`
	case "codex":
		// The @openai/codex npm package ships prebuilt musl binaries, so it
		// installs cleanly on the Alpine base.
		return `# --- Codex ---
RUN apk add --no-cache git nodejs npm ripgrep && \
    npm install -g @openai/codex`
	default:
		return fmt.Sprintf(`# --- TODO: install the %s CLI ---
# Aileron does not yet ship a verified install recipe for %s.
# Replace this stub with an install that puts %q on PATH and uses apk
# against the Alpine base. See:
#   https://docs.withaileron.ai/development/sandbox-agent-images/
# RUN apk add --no-cache ...`, agent, agent, agent)
	}
}
