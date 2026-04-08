package launch_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/model"
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
	result := launch.EvaluateCommand(path, "git push origin main", "/tmp")
	if result.Disposition != model.DispositionRequireApproval {
		t.Errorf("expected require_approval, got %q", result.Disposition)
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
	launch.WriteDeny(&buf, "rm -rf /", "no recursive delete")
	out := buf.String()
	if !strings.Contains(out, "denied") {
		t.Error("expected 'denied' in output")
	}
	if !strings.Contains(out, "no recursive delete") {
		t.Error("expected reason in output")
	}
}

func TestWriteDenyByUser(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDenyByUser(&buf, "git push")
	if !strings.Contains(buf.String(), "denied by user") {
		t.Error("expected 'denied by user' in output")
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

func TestWriteDeny_NoReason(t *testing.T) {
	var buf bytes.Buffer
	launch.WriteDeny(&buf, "bad command", "")
	out := buf.String()
	if !strings.Contains(out, "denied") {
		t.Error("expected 'denied' in output")
	}
	// Should not have an extra line for empty reason.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line with no reason, got %d", len(lines))
	}
}
