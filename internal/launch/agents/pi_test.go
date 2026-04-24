package agents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestPi(t *testing.T) {
	p := agents.Pi{}

	if p.Name() != "pi" {
		t.Errorf("expected name 'pi', got %q", p.Name())
	}

	names := p.BinaryNames()
	if len(names) != 1 || names[0] != "pi" {
		t.Errorf("expected BinaryNames [\"pi\"], got %v", names)
	}

	args := p.Args()
	if len(args) < 2 {
		t.Fatal("expected Args with --tools")
	}
	if args[0] != "--tools" {
		t.Errorf("expected first arg '--tools', got %q", args[0])
	}

	env := p.Env()
	if env == nil {
		t.Fatal("expected non-nil Env")
	}
	if env["AILERON_REAL_SHELL"] != "/bin/bash" {
		t.Errorf("expected AILERON_REAL_SHELL=/bin/bash, got %q", env["AILERON_REAL_SHELL"])
	}
}

func TestPi_NormalizeCommand_Passthrough(t *testing.T) {
	p := agents.Pi{}

	cmd, eval := p.NormalizeCommand("echo hello")
	if !eval {
		t.Fatal("expected evaluate=true")
	}
	if cmd != "echo hello" {
		t.Errorf("expected 'echo hello', got %q", cmd)
	}
}

func TestPi_NormalizeCommand_ComplexCommand(t *testing.T) {
	p := agents.Pi{}

	complex := "git push origin main && echo done"
	cmd, eval := p.NormalizeCommand(complex)
	if !eval {
		t.Fatal("expected evaluate=true")
	}
	if cmd != complex {
		t.Errorf("expected command unchanged, got %q", cmd)
	}
}

func TestPi_ConfigureShell_WritesSettings(t *testing.T) {
	p := agents.Pi{}
	dir := t.TempDir()
	shimPath := "/usr/local/bin/aileron-sh"

	if err := p.ConfigureShell(shimPath, dir); err != nil {
		t.Fatalf("ConfigureShell failed: %v", err)
	}

	settingsPath := filepath.Join(dir, ".pi", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("expected settings file at %s: %v", settingsPath, err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if settings["shellPath"] != shimPath {
		t.Errorf("expected shellPath %q, got %v", shimPath, settings["shellPath"])
	}
}

func TestPi_ConfigureShell_DefaultsToWorkingDir(t *testing.T) {
	p := agents.Pi{}
	// Empty dir should fall back to os.Getwd(). Since we can't predict
	// the cwd in tests reliably, just verify it doesn't error.
	// The settings file will be written relative to cwd.
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	if err := p.ConfigureShell("/usr/local/bin/aileron-sh", ""); err != nil {
		t.Fatalf("ConfigureShell with empty dir failed: %v", err)
	}

	settingsPath := filepath.Join(dir, ".pi", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("expected settings file at %s: %v", settingsPath, err)
	}
}

func TestPi_ConfigureShell_UnwritableDir(t *testing.T) {
	p := agents.Pi{}
	// Point at a path that can't be created.
	err := p.ConfigureShell("/usr/local/bin/aileron-sh", "/dev/null/impossible")
	if err == nil {
		t.Fatal("expected error for unwritable directory")
	}
}

func TestPi_ConfigureShell_PreservesExistingSettings(t *testing.T) {
	p := agents.Pi{}
	dir := t.TempDir()
	piDir := filepath.Join(dir, ".pi")
	os.MkdirAll(piDir, 0o755)

	// Write existing settings with other fields.
	existing := map[string]any{"thinking": "high", "theme": "dark"}
	data, _ := json.Marshal(existing)
	os.WriteFile(filepath.Join(piDir, "settings.json"), data, 0o644)

	shimPath := "/usr/local/bin/aileron-sh"
	if err := p.ConfigureShell(shimPath, dir); err != nil {
		t.Fatalf("ConfigureShell failed: %v", err)
	}

	result, _ := os.ReadFile(filepath.Join(piDir, "settings.json"))
	var settings map[string]any
	json.Unmarshal(result, &settings)

	if settings["shellPath"] != shimPath {
		t.Errorf("expected shellPath %q, got %v", shimPath, settings["shellPath"])
	}
	if settings["thinking"] != "high" {
		t.Errorf("expected existing 'thinking' preserved, got %v", settings["thinking"])
	}
	if settings["theme"] != "dark" {
		t.Errorf("expected existing 'theme' preserved, got %v", settings["theme"])
	}
}
