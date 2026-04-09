package launch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestDetectLanguage_Go(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0o644)
	if got := launch.DetectLanguage(dir); got != "go" {
		t.Errorf("expected 'go', got %q", got)
	}
}

func TestDetectLanguage_Node(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644)
	if got := launch.DetectLanguage(dir); got != "node" {
		t.Errorf("expected 'node', got %q", got)
	}
}

func TestDetectLanguage_Python(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), nil, 0o644)
	if got := launch.DetectLanguage(dir); got != "python" {
		t.Errorf("expected 'python', got %q", got)
	}
}

func TestDetectLanguage_Rust(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Cargo.toml"), nil, 0o644)
	if got := launch.DetectLanguage(dir); got != "rust" {
		t.Errorf("expected 'rust', got %q", got)
	}
}

func TestDetectLanguage_None(t *testing.T) {
	dir := t.TempDir()
	if got := launch.DetectLanguage(dir); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestInitPolicy_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0o644)

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
	if !strings.Contains(content, "go test") {
		t.Error("expected Go-specific allow rules")
	}
	if !strings.Contains(content, "rm -rf") {
		t.Error("expected deny rules")
	}
	if !strings.Contains(content, "AWS_*") {
		t.Error("expected env scrub rules")
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

func TestInitPolicy_NoLanguage(t *testing.T) {
	dir := t.TempDir()

	path, err := launch.InitPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	// Should still have git commands but not language-specific ones.
	if !strings.Contains(content, "git status") {
		t.Error("expected git allow rules")
	}
	if strings.Contains(content, "go test") {
		t.Error("should not have Go rules without go.mod")
	}
}

func TestInitPolicy_NodeProject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644)

	path, err := launch.InitPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "npm") {
		t.Error("expected Node-specific allow rules")
	}
}
