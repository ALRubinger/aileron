package launch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/policy/launch"
)

func TestLoadUserSettings_NoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	pf, err := launch.LoadUserSettings()
	if err != nil {
		t.Fatalf("expected no error when settings file absent, got %v", err)
	}
	if pf.Version != 1 {
		t.Errorf("Version = %d, want 1", pf.Version)
	}
	if len(pf.Allow) != 0 {
		t.Errorf("expected no rules, got %d allow rules", len(pf.Allow))
	}
}

func TestLoadUserSettings_ValidFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)

	settings := `
version: 1
default: allow
allow:
  - "cat /tmp/*"
  - "ls /tmp/*"
ask:
  - command: "rm /tmp/*"
    description: "confirm before deleting temp files"
settings:
  timeout: 60
`
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(settings), 0o644)

	pf, err := launch.LoadUserSettings()
	if err != nil {
		t.Fatalf("LoadUserSettings failed: %v", err)
	}
	if len(pf.Allow) != 2 {
		t.Errorf("Allow = %d, want 2", len(pf.Allow))
	}
	if len(pf.Ask) != 1 {
		t.Errorf("Ask = %d, want 1", len(pf.Ask))
	}
	if pf.Default != "allow" {
		t.Errorf("Default = %q, want 'allow'", pf.Default)
	}
	if pf.Settings == nil || pf.Settings.Timeout != 60 {
		t.Errorf("expected Timeout=60, got %v", pf.Settings)
	}
}

func TestLoadUserSettings_NoHomeDir(t *testing.T) {
	// When HOME is empty/unset, UserHomeDir may fail on some systems.
	// LoadUserSettings should return an empty policy, not an error.
	t.Setenv("HOME", "")

	pf, err := launch.LoadUserSettings()
	if err != nil {
		t.Fatalf("expected no error when HOME is empty, got %v", err)
	}
	if pf.Version != 1 {
		t.Errorf("Version = %d, want 1", pf.Version)
	}
}

func TestLoadUserSettings_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte("{{invalid"), 0o644)

	_, err := launch.LoadUserSettings()
	if err == nil {
		t.Fatal("expected error for invalid YAML in user settings")
	}
}

func TestLoadWithProfiles_UserSettingsAsBase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write user settings with personal allow rules.
	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(`
version: 1
allow:
  - "cat /tmp/*"
settings:
  timeout: 120
`), 0o644)

	// Write a project policy.
	projectDir := filepath.Join(dir, "project")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, "aileron.yaml"), []byte(`
version: 1
default: ask
allow:
  - "go test ./..."
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
settings:
  timeout: 30
`), 0o644)

	pf, err := launch.LoadWithProfiles(filepath.Join(projectDir, "aileron.yaml"))
	if err != nil {
		t.Fatalf("LoadWithProfiles failed: %v", err)
	}

	// Should contain user allow + project allow + defaults.
	defaults := launch.DefaultPolicy()
	minAllow := len(defaults.Allow) + 2 // 1 user + 1 project
	if len(pf.Allow) < minAllow {
		t.Errorf("Allow = %d, want at least %d (defaults + user + project)", len(pf.Allow), minAllow)
	}

	// Deny includes defaults + project.
	minDeny := len(defaults.Deny) + 1
	if len(pf.Deny) < minDeny {
		t.Errorf("Deny = %d, want at least %d (defaults + project)", len(pf.Deny), minDeny)
	}

	// Project timeout (30) should override user timeout (120).
	if pf.Settings == nil || pf.Settings.Timeout != 30 {
		t.Errorf("Timeout = %v, want 30 (project wins)", pf.Settings)
	}
}

func TestLoadWithProfiles_ProjectOverridesUserDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(`
version: 1
default: allow
`), 0o644)

	projectDir := filepath.Join(dir, "project")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, "aileron.yaml"), []byte(`
version: 1
default: deny
`), 0o644)

	pf, err := launch.LoadWithProfiles(filepath.Join(projectDir, "aileron.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if pf.Default != "deny" {
		t.Errorf("Default = %q, want 'deny' (project wins)", pf.Default)
	}
}

func TestLoadWithProfiles_NoUserSettings(t *testing.T) {
	// With no ~/.aileron/settings.yaml, LoadWithProfiles should still work.
	t.Setenv("HOME", t.TempDir())

	pf, err := launch.LoadWithProfiles(testdataPath("basic.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Should have defaults + basic.yaml's 4 allow rules.
	defaults := launch.DefaultPolicy()
	minExpected := len(defaults.Allow) + 4
	if len(pf.Allow) < minExpected {
		t.Errorf("Allow = %d, want at least %d (defaults + basic.yaml)", len(pf.Allow), minExpected)
	}
}
