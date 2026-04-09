package launch

import (
	"fmt"
	"os"
	"path/filepath"
)

// languageDetectors maps marker files to language names.
var languageDetectors = []struct {
	marker   string
	language string
}{
	{"go.mod", "go"},
	{"go.work", "go"},
	{"package.json", "node"},
	{"Cargo.toml", "rust"},
	{"pyproject.toml", "python"},
	{"requirements.txt", "python"},
	{"Gemfile", "ruby"},
	{"mix.exs", "elixir"},
	{"build.gradle", "java"},
	{"pom.xml", "java"},
}

// DetectLanguage examines the directory for known marker files and
// returns the detected language name, or "" if none is found.
func DetectLanguage(dir string) string {
	for _, d := range languageDetectors {
		if _, err := os.Stat(filepath.Join(dir, d.marker)); err == nil {
			return d.language
		}
	}
	return ""
}

// InitPolicy writes a starter aileron.yaml to the given directory.
// If the file already exists, it returns an error rather than
// overwriting. The generated content includes language-specific
// examples when a language is detected.
func InitPolicy(dir string) (string, error) {
	path := filepath.Join(dir, "aileron.yaml")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("aileron.yaml already exists in %s", dir)
	}

	lang := DetectLanguage(dir)
	content := generatePolicyYAML(lang)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing aileron.yaml: %w", err)
	}
	return path, nil
}

func generatePolicyYAML(lang string) string {
	header := `# Aileron policy — checked into the repo, shared with the team.
# Docs: https://github.com/ALRubinger/aileron
version: 1
default: ask
`

	allow := languageAllowRules(lang)

	deny := `
deny:
  - command: "rm -rf *"
    description: "no broad recursive delete"
  - command: "git push origin main"
    description: "no direct push to main"
  - command: "git push origin master"
    description: "no direct push to master"
`

	ask := `
ask:
  - "git push *"
  - "git commit *"
`

	env := `
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

	return header + allow + deny + ask + env
}

func languageAllowRules(lang string) string {
	switch lang {
	case "go":
		return `
allow:
  - "go test *"
  - "go build *"
  - "go vet *"
  - "go mod *"
  - "go run *"
  - "task *"
  - "git status"
  - "git diff *"
  - "git log *"
  - "gh pr *"
  - "gh issue *"
`
	case "node":
		return `
allow:
  - "npm test *"
  - "npm run *"
  - "npm install *"
  - "npx *"
  - "pnpm *"
  - "yarn *"
  - "git status"
  - "git diff *"
  - "git log *"
  - "gh pr *"
  - "gh issue *"
`
	case "python":
		return `
allow:
  - "python *"
  - "pip install *"
  - "pytest *"
  - "uv *"
  - "git status"
  - "git diff *"
  - "git log *"
  - "gh pr *"
  - "gh issue *"
`
	case "rust":
		return `
allow:
  - "cargo test *"
  - "cargo build *"
  - "cargo run *"
  - "cargo clippy *"
  - "cargo fmt *"
  - "git status"
  - "git diff *"
  - "git log *"
  - "gh pr *"
  - "gh issue *"
`
	default:
		return `
allow:
  - "git status"
  - "git diff *"
  - "git log *"
  - "gh pr *"
  - "gh issue *"
`
	}
}
