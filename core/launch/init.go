package launch

import (
	"fmt"
	"os"
	"path/filepath"
)

// InitPolicy writes a starter aileron.yaml to the given directory.
// If the file already exists, it returns an error rather than
// overwriting. The generated file is minimal — language toolchain
// rules and OS-specific rules are compiled into the Aileron binary
// as built-in defaults (see core/policy/launch/defaults.go).
func InitPolicy(dir string) (string, error) {
	path := filepath.Join(dir, "aileron.yaml")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("aileron.yaml already exists in %s", dir)
	}

	content := generatePolicyYAML()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing aileron.yaml: %w", err)
	}
	return path, nil
}

func generatePolicyYAML() string {
	return `# Aileron policy — checked into the repo, shared with the team.
#
# Language toolchain rules (go, node, python, rust, etc.) and OS-specific
# rules are built into Aileron. You only need to add project-specific
# rules here.
#
# Three-layer merge: built-in defaults → this file → ~/.aileron/settings.yaml
# Docs: https://github.com/ALRubinger/aileron
version: 1
default: ask

deny:
  - command: "git push origin main"
    description: "no direct push to main"
  - command: "git push origin master"
    description: "no direct push to master"

ask:
  - "git push *"
  - "git commit *"

env:
  scrub:
    - "AWS_*"
    - "*_SECRET"
    - "*_SECRET_*"
    - "*_KEY"
    - "*_TOKEN"
    - "OPENAI_*"
    - "ANTHROPIC_*"
  passthrough:
    - "HOME"
    - "PATH"
    - "TERM"
    - "LANG"
    - "USER"
`
}
