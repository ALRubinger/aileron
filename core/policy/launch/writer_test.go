package launch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/policy/launch"
)

func TestAppendAllowRule_NewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")

	if err := launch.AppendAllowRule(path, "echo hello"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "version: 1") {
		t.Error("expected version header")
	}
	if !strings.Contains(content, `"echo hello"`) {
		t.Errorf("expected quoted pattern in file, got:\n%s", content)
	}
}

func TestAppendAllowRule_ExistingBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aileron.yaml")
	os.WriteFile(path, []byte(`version: 1
default: ask
allow:
  - "go test ./..."
  - "go build ./..."
deny:
  - command: "rm -rf *"
`), 0o644)

	if err := launch.AppendAllowRule(path, "echo hello"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	// New entry should appear after the existing allow entries.
	if !strings.Contains(content, `  - "echo hello"`) {
		t.Errorf("expected new entry, got:\n%s", content)
	}

	// Deny block should still be intact.
	if !strings.Contains(content, `deny:`) {
		t.Errorf("deny block was lost:\n%s", content)
	}

	// Verify order: new entry should come after "go build" and before "deny:"
	goIdx := strings.Index(content, "go build")
	echoIdx := strings.Index(content, "echo hello")
	denyIdx := strings.Index(content, "deny:")
	if echoIdx <= goIdx || echoIdx >= denyIdx {
		t.Errorf("entry not in correct position:\n%s", content)
	}
}

func TestAppendAllowRule_NoAllowBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aileron.yaml")
	os.WriteFile(path, []byte(`version: 1
default: deny
deny:
  - command: "rm -rf *"
`), 0o644)

	if err := launch.AppendAllowRule(path, "ls -la"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "allow:") {
		t.Errorf("expected allow block to be created:\n%s", content)
	}
	if !strings.Contains(content, `"ls -la"`) {
		t.Errorf("expected pattern in allow block:\n%s", content)
	}
	// Original deny should still be there.
	if !strings.Contains(content, "deny:") {
		t.Errorf("deny block was lost:\n%s", content)
	}
}

func TestAppendAllowRule_PreservesFormatting(t *testing.T) {
	original := `# Project policy
version: 1
default: ask

# Safe commands
allow:
  - "go test ./..."
  - "go build ./..."

# Dangerous commands
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`
	path := filepath.Join(t.TempDir(), "aileron.yaml")
	os.WriteFile(path, []byte(original), 0o644)

	launch.AppendAllowRule(path, "cat README.md")

	data, _ := os.ReadFile(path)
	content := string(data)

	// Comments should be preserved.
	if !strings.Contains(content, "# Project policy") {
		t.Error("top comment lost")
	}
	if !strings.Contains(content, "# Dangerous commands") {
		t.Error("deny comment lost")
	}
}

func TestAppendAllowRule_MultipleAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")

	launch.AppendAllowRule(path, "echo one")
	launch.AppendAllowRule(path, "echo two")
	launch.AppendAllowRule(path, "echo three")

	data, _ := os.ReadFile(path)
	content := string(data)

	if strings.Count(content, `  - "echo`) != 3 {
		t.Errorf("expected 3 entries, got:\n%s", content)
	}
}

func TestAppendAllowRule_CreatesSubdirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".aileron", "settings.yaml")

	if err := launch.AppendAllowRule(path, "echo hello"); err != nil {
		t.Fatalf("expected directory to be created: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}
