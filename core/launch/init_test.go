package launch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestInitPolicy_CreatesMinimalFile(t *testing.T) {
	dir := t.TempDir()

	path, err := launch.InitPolicy(dir)
	if err != nil {
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
	if !strings.Contains(content, "default: ask") {
		t.Error("expected default disposition")
	}
	if !strings.Contains(content, "AWS_*") {
		t.Error("expected env scrub rules")
	}
	if !strings.Contains(content, "built into Aileron") {
		t.Error("expected comment explaining built-in defaults")
	}
	// Should NOT contain language-specific allow rules — those are built-in.
	if strings.Contains(content, "go test") {
		t.Error("should not contain language-specific rules (they're built-in)")
	}
	if strings.Contains(content, "npm") {
		t.Error("should not contain language-specific rules (they're built-in)")
	}
}

func TestInitPolicy_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1"), 0o644)

	_, err := launch.InitPolicy(dir)
	if err == nil {
		t.Fatal("expected error when aileron.yaml already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got %v", err)
	}
}

func TestInitPolicy_FileContent(t *testing.T) {
	dir := t.TempDir()

	path, err := launch.InitPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	// Verify the three-layer merge comment is present.
	if !strings.Contains(content, "Three-layer") || !strings.Contains(content, "settings.yaml") {
		t.Error("expected three-layer merge documentation comment")
	}
	// Verify env scrub present.
	if !strings.Contains(content, "AWS_*") {
		t.Error("expected env scrub rules")
	}
}
