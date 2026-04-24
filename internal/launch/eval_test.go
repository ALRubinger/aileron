package launch_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/model"
)

func writePolicyFile(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "aileron.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEvaluateCommand_Allow(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: deny
allow:
  - "echo *"
`)
	result := launch.EvaluateCommand(path, "echo hello", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_Deny(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)
	result := launch.EvaluateCommand(path, "rm -rf /important", "/tmp")
	if result.Disposition != model.DispositionDeny {
		t.Errorf("expected deny, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_Ask(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: ask
`)
	// Use a command not in any built-in allow or deny list.
	result := launch.EvaluateCommand(path, "docker run myimage", "/tmp")
	if result.Disposition != model.DispositionRequireApproval {
		t.Errorf("expected require_approval, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_BuiltinDenyRepoDelete(t *testing.T) {
	// gh repo delete is denied by built-in defaults even without a project policy.
	path := writePolicyFile(t, `
version: 1
default: allow
`)
	result := launch.EvaluateCommand(path, "gh repo delete my-org/my-repo", "/tmp")
	if result.Disposition != model.DispositionDeny {
		t.Errorf("expected deny for gh repo delete, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_BuiltinDenyRmRfRoot(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: allow
`)
	result := launch.EvaluateCommand(path, "rm -rf /", "/tmp")
	if result.Disposition != model.DispositionDeny {
		t.Errorf("expected deny for rm -rf /, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_ProjectOverridesBuiltinDeny(t *testing.T) {
	// Per ADR-0016, the project layer can override built-in default deny
	// rules by defining the same command pattern. Later layer wins.
	path := writePolicyFile(t, `
version: 1
default: allow
allow:
  - "gh repo delete *"
  - "rm -rf /*"
`)
	result := launch.EvaluateCommand(path, "gh repo delete my-org/repo", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected project allow to override built-in deny, got %q", result.Disposition)
	}
	result = launch.EvaluateCommand(path, "rm -rf /", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected project allow to override built-in deny for rm -rf, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_DenyOverridesAllow(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: allow
allow:
  - "git *"
deny:
  - command: "git push origin main"
    description: "no push to main"
`)
	result := launch.EvaluateCommand(path, "git push origin main", "/tmp")
	if result.Disposition != model.DispositionDeny {
		t.Errorf("expected deny to override allow, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_EmptyPath(t *testing.T) {
	// Empty path → empty policy → default ask.
	result := launch.EvaluateCommand("", "anything", "/tmp")
	if result.Disposition != model.DispositionRequireApproval {
		t.Errorf("expected ask with empty path, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_DefaultAllow(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: allow
`)
	result := launch.EvaluateCommand(path, "anything-at-all", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_BinaryAndArgs(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: deny
allow:
  - binary: "go"
`)
	// "go test ./..." has binary "go".
	result := launch.EvaluateCommand(path, "go test ./...", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow by binary match, got %q", result.Disposition)
	}
}

func TestFindPolicyFile_Found(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1"), 0o644)

	path := launch.FindPolicyFile(dir)
	if path == "" {
		t.Fatal("expected to find aileron.yaml")
	}
	if !strings.HasSuffix(path, "aileron.yaml") {
		t.Errorf("expected path ending in aileron.yaml, got %q", path)
	}
}

func TestFindPolicyFile_WalksUp(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "subdir")
	os.MkdirAll(child, 0o755)
	os.WriteFile(filepath.Join(parent, "aileron.yaml"), []byte("version: 1"), 0o644)

	path := launch.FindPolicyFile(child)
	if path == "" {
		t.Fatal("expected to find aileron.yaml in parent")
	}
}

func TestFindPolicyFile_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := launch.FindPolicyFile(dir)
	if path != "" {
		t.Errorf("expected empty string, got %q", path)
	}
}

func TestWriteDeny(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "rm -rf /", "no recursive delete", "deny_0", "/project/aileron.yaml")
	out := buf.String()
	if !strings.Contains(out, "Denied") {
		t.Error("expected 'Denied' in output")
	}
	if !strings.Contains(out, "no recursive delete") {
		t.Error("expected reason in output")
	}
	if !strings.Contains(out, "/project/aileron.yaml") {
		t.Errorf("expected policy path in output, got %q", out)
	}
}

func TestWriteDenyByUser(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDenyByUser(&buf, "git push")
	if !strings.Contains(buf.String(), "Denied") {
		t.Error("expected 'Denied' in output")
	}
}

func TestEvaluateCommand_InvalidPolicyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aileron.yaml")
	os.WriteFile(path, []byte("{{invalid yaml"), 0o644)

	// Invalid policy file → falls back to empty policy → default ask.
	result := launch.EvaluateCommand(path, "echo hello", "/tmp")
	if result.Disposition != model.DispositionRequireApproval {
		t.Errorf("expected ask fallback for invalid policy, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_NoPolicyFileNoHome(t *testing.T) {
	// When HOME is empty and no policy path, built-in defaults still apply.
	t.Setenv("HOME", "")
	// "echo *" is in the built-in allow list.
	result := launch.EvaluateCommand("", "echo hello", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow from built-in defaults, got %q", result.Disposition)
	}
	// An unknown command should get default ask.
	result = launch.EvaluateCommand("", "docker run myimage", "/tmp")
	if result.Disposition != model.DispositionRequireApproval {
		t.Errorf("expected ask for unknown command, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_NoPolicyFileInvalidUserSettings(t *testing.T) {
	// Invalid user settings → falls back to defaults only.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte("{{invalid"), 0o644)

	// No project policy path + invalid user settings → built-in defaults apply.
	// "echo *" is in the built-in allow list.
	result := launch.EvaluateCommand("", "echo hello", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow from built-in defaults, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_InvalidUserSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte("{{invalid"), 0o644)

	// Invalid user settings with a project policy → should fall back.
	projectDir := filepath.Join(dir, "project")
	os.MkdirAll(projectDir, 0o755)
	policyPath := filepath.Join(projectDir, "aileron.yaml")
	os.WriteFile(policyPath, []byte("version: 1\ndefault: allow\n"), 0o644)

	result := launch.EvaluateCommand(policyPath, "echo hello", projectDir)
	// LoadWithProfiles fails due to bad user settings → fallback to empty → default ask.
	if result.Disposition != model.DispositionRequireApproval {
		t.Errorf("expected ask fallback for invalid user settings, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_NoPolicyFileUsesUserSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write user settings that deny "rm *".
	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(`
version: 1
default: allow
deny:
  - command: "rm *"
    description: "user denies rm"
`), 0o644)

	// Empty policy path → should still load user settings.
	result := launch.EvaluateCommand("", "rm /tmp/foo", "/tmp")
	if result.Disposition != model.DispositionDeny {
		t.Errorf("expected deny from user settings, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_UserSettingsMergedWithProject(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// User settings allow cat.
	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(`
version: 1
allow:
  - "cat *"
`), 0o644)

	// Project denies everything by default but allows echo.
	projectDir := filepath.Join(dir, "project")
	os.MkdirAll(projectDir, 0o755)
	policyPath := filepath.Join(projectDir, "aileron.yaml")
	os.WriteFile(policyPath, []byte(`
version: 1
default: deny
allow:
  - "echo *"
`), 0o644)

	// "cat" should be allowed (from user settings).
	result := launch.EvaluateCommand(policyPath, "cat /etc/hosts", projectDir)
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow from user settings for cat, got %q", result.Disposition)
	}

	// "echo" should be allowed (from project policy).
	result = launch.EvaluateCommand(policyPath, "echo hello", projectDir)
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow from project for echo, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_NormalizesAbsoluteBinaryPath(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: deny
allow:
  - "ls *"
`)
	// Agent calls "/bin/ls -la /tmp" — should still match "ls *".
	result := launch.EvaluateCommand(path, "/bin/ls -la /tmp", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow after path normalization, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_NormalizesBinaryForDenyRule(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)
	// Agent calls "/usr/bin/rm -rf /important" — should still be denied.
	result := launch.EvaluateCommand(path, "/usr/bin/rm -rf /important", "/tmp")
	if result.Disposition != model.DispositionDeny {
		t.Errorf("expected deny after path normalization, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_NormalizesBinaryField(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: deny
allow:
  - binary: "go"
`)
	// Agent calls "/usr/local/go/bin/go test ./..." — binary should normalize to "go".
	result := launch.EvaluateCommand(path, "/usr/local/go/bin/go test ./...", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow by normalized binary match, got %q", result.Disposition)
	}
}

func TestEvaluateCommand_BareBinaryUnchanged(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: deny
allow:
  - "echo *"
`)
	// Bare binary (no path prefix) should still work as before.
	result := launch.EvaluateCommand(path, "echo hello", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Errorf("expected allow for bare binary, got %q", result.Disposition)
	}
}

func TestWriteDeny_BuiltinRule(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "eval bad", "", "builtin_eval", "")
	if !strings.Contains(buf.String(), "built-in defaults") {
		t.Errorf("expected 'built-in defaults' source, got %q", buf.String())
	}
}

func TestWriteDeny_PersonalSettings(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "curl", "blocked", "deny_0", "/Users/me/.aileron/settings.yaml")
	if !strings.Contains(buf.String(), "personal settings") {
		t.Errorf("expected 'personal settings' source, got %q", buf.String())
	}
}

func TestWriteDeny_ProjectPath(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "rm -rf /", "dangerous", "deny_0", "/repo/aileron.yaml")
	out := buf.String()
	if !strings.Contains(out, "/repo/aileron.yaml") {
		t.Errorf("expected project path in output, got %q", out)
	}
	if !strings.Contains(out, "Policy:") {
		t.Error("expected 'Policy:' label")
	}
}

func TestWriteDeny_NoSource(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "echo", "", "", "")
	out := buf.String()
	if strings.Contains(out, "Policy:") {
		t.Error("should not show 'Policy:' when no source")
	}
}

func TestWriteDeny_NoReason(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "bad command", "", "", "")
	out := buf.String()
	if !strings.Contains(out, "Denied") {
		t.Error("expected 'Denied' in output")
	}
	// Should not have an extra line for empty reason.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line with no reason, got %d", len(lines))
	}
}

// --- Layer extraction and override hint tests (ADR-0016) ---

func TestEvalResult_LayerFromRuleID(t *testing.T) {
	path := writePolicyFile(t, `
version: 1
default: deny
allow:
  - "echo *"
`)
	result := launch.EvaluateCommand(path, "echo hello", "/tmp")
	if result.Disposition != model.DispositionAllow {
		t.Fatalf("expected allow, got %q", result.Disposition)
	}
	// Rule ID should have a layer prefix from LoadWithProfiles.
	if result.Layer == "" {
		t.Error("expected non-empty layer in EvalResult")
	}
}

func TestWriteDeny_DefaultLayerOverrideHint(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "security dump", "blocked", "default:deny_0", "")
	out := buf.String()
	if !strings.Contains(out, "built-in defaults") {
		t.Errorf("expected 'built-in defaults' source, got %q", out)
	}
	if !strings.Contains(out, "override in aileron.yaml") {
		t.Errorf("expected override hint, got %q", out)
	}
}

func TestWriteDeny_ProjectLayerOverrideHint(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "deploy", "blocked", "project:deny_0", "/repo/aileron.yaml")
	out := buf.String()
	if !strings.Contains(out, "/repo/aileron.yaml") {
		t.Errorf("expected project path, got %q", out)
	}
	if !strings.Contains(out, "override in ~/.aileron/settings.yaml") {
		t.Errorf("expected user override hint, got %q", out)
	}
}

func TestWriteDeny_UserLayerNoOverrideHint(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "deploy", "blocked", "user:deny_0", "")
	out := buf.String()
	if strings.Contains(out, "Hint:") {
		t.Errorf("user layer should not have override hint, got %q", out)
	}
}

func TestEvaluateCommand_SpecificDenyBeatsGeneralAllow(t *testing.T) {
	// "rm -rf /" (deny, 8 literal chars) should beat "rm *" (allow, 3 literal chars).
	path := writePolicyFile(t, `
version: 1
default: ask
allow:
  - "rm *"
deny:
  - command: "rm -rf /"
    description: "block rm -rf root"
`)
	result := launch.EvaluateCommand(path, "rm -rf /", "/tmp")
	if result.Disposition != model.DispositionDeny {
		t.Errorf("expected specific deny to beat general allow, got %q", result.Disposition)
	}
}
