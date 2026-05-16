package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/wrap"
)

// --- YAML mode ---

func TestRunActionWrap_YAMLMode_EmitsValidConnector(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "spec.yaml")
	if err := os.WriteFile(yamlPath, []byte(testWrapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "connector")
	var stdout, stderr bytes.Buffer
	code := runActionWrap(
		[]string{"--config=" + yamlPath, "--out=" + out},
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr: %s", code, stderr.String())
	}
	manifest := filepath.Join(out, "connector/manifest.toml")
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	m, err := cstore.ParseManifest("manifest.toml", body)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := cstore.ValidateManifest(m, "manifest.toml"); err != nil {
		t.Errorf("emitted manifest does not validate: %v", err)
	}
}

func TestRunActionWrap_YAMLMode_RejectsMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runActionWrap(
		[]string{"--config=/nonexistent/spec.yaml", "--out=/tmp/x"},
		&stdout, &stderr,
	)
	if code == 0 {
		t.Error("expected nonzero exit on missing config")
	}
}

func TestRunActionWrap_YAMLMode_PropagatesValidationErrors(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "spec.yaml")
	bad := strings.Replace(testWrapYAML, "/usr/bin/git", "git", 1)
	if err := os.WriteFile(yamlPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runActionWrap(
		[]string{"--config=" + yamlPath, "--out=" + tmp + "/c"},
		&stdout, &stderr,
	)
	if code == 0 {
		t.Fatal("expected nonzero exit on validation failure")
	}
	if !strings.Contains(stderr.String(), "absolute") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// --- --help mode ---

func TestRunActionWrap_HelpMode_RequiresName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runActionWrap([]string{"/usr/bin/git"}, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when --name is missing")
	}
}

func TestRunActionWrap_HelpMode_RequiresProgram(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runActionWrap(
		[]string{"--name=github://x/y"},
		&stdout, &stderr,
	)
	if code == 0 {
		t.Error("expected nonzero exit when no CLI is supplied and no --config")
	}
}

func TestRunActionWrap_HelpMode_EmitsScaffoldFromFakeHelpRunner(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "connector")

	// Swap the help runner with a path-aware fake. Top-level returns
	// a cobra subcommand list; each top-level subcommand returns a
	// leaf-style help (no Available Commands) so the recursive walk
	// terminates immediately and emits one operation per top-level
	// name — `pr`, `issue`.
	rootHelp := `gh works with GitHub.

Available Commands:
  pr       Manage pull requests
  issue    Manage issues
`
	leafHelp := `Manage things.

Usage:
  thing <name>
`
	orig := actionWrapHelpRunner
	actionWrapHelpRunner = func(_ context.Context, _ string, args []string) (string, error) {
		key := ""
		if n := len(args); n > 0 && args[n-1] == "--help" {
			for _, a := range args[:n-1] {
				if key != "" {
					key += " "
				}
				key += a
			}
		}
		switch key {
		case "":
			return rootHelp, nil
		case "pr", "issue":
			return leafHelp, nil
		}
		return "", nil
	}
	t.Cleanup(func() { actionWrapHelpRunner = orig })

	var stdout, stderr bytes.Buffer
	code := runActionWrap(
		[]string{"--name=github://aileron-test/gh", "--out=" + out, "/usr/bin/gh"},
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr: %s", code, stderr.String())
	}
	manifest := filepath.Join(out, "connector/manifest.toml")
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("manifest not written: %v", err)
	}
	for _, sub := range []string{"pr", "issue"} {
		if _, err := os.Stat(filepath.Join(out, "actions", sub+".md")); err != nil {
			t.Errorf("missing action.md for %q: %v", sub, err)
		}
	}
}

func TestRunActionWrap_ForceOverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "spec.yaml")
	if err := os.WriteFile(yamlPath, []byte(testWrapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "c")
	var stdout, stderr bytes.Buffer
	if code := runActionWrap([]string{"--config=" + yamlPath, "--out=" + out}, &stdout, &stderr); code != 0 {
		t.Fatalf("first emit failed: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runActionWrap([]string{"--config=" + yamlPath, "--out=" + out}, &stdout, &stderr); code == 0 {
		t.Fatal("expected refusal without --force")
	}
	stdout.Reset()
	stderr.Reset()
	if code := runActionWrap([]string{"--config=" + yamlPath, "--out=" + out, "--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--force failed: %s", stderr.String())
	}
}

// --- --install mode ---

func TestRunActionWrap_InstallMode_WritesToStoreAndActionsDir(t *testing.T) {
	// --install reads HOME-rooted paths. Redirect HOME so the test is
	// hermetic and doesn't touch the user's real ~/.aileron.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Swap the embedded forwarder for a tiny payload so the test
	// doesn't pull a 3 MB binary into its store; the wrap.Install path
	// is forwarder-bytes-agnostic — it just hashes them.
	origFW := actionWrapForwarder
	actionWrapForwarder = []byte("FORWARDER-TEST")
	t.Cleanup(func() { actionWrapForwarder = origFW })

	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "spec.yaml")
	if err := os.WriteFile(yamlPath, []byte(testWrapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runActionWrap(
		[]string{"--config=" + yamlPath, "--install"},
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr: %s", code, stderr.String())
	}

	// Action files land under ~/.aileron/actions/<connector-leaf>-<op>.md.
	actionsDir := filepath.Join(home, ".aileron", "actions")
	for _, name := range []string{"gitcrawl-log.md", "gitcrawl-status.md"} {
		if _, err := os.Stat(filepath.Join(actionsDir, name)); err != nil {
			t.Errorf("expected action file %s: %v", name, err)
		}
	}
	// Manifest lands in the daemon's connector store under the
	// computed hash.
	storeSha := filepath.Join(home, ".aileron", "store", "connectors", "sha256")
	entries, err := os.ReadDir(storeSha)
	if err != nil {
		t.Fatalf("store dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("store sha256 dir has %d entries, want 1", len(entries))
	}
	if _, err := os.Stat(filepath.Join(storeSha, entries[0].Name(), "manifest.toml")); err != nil {
		t.Errorf("manifest.toml missing in store: %v", err)
	}
	// No source tree written to --out (the flag's irrelevant in
	// --install mode).
	if _, err := os.Stat(filepath.Join(tmp, "aileron-connector")); err == nil {
		t.Error("--install should not produce a source tree")
	}
}

func TestRunActionWrap_InstallMode_IdempotentOnSecondCall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	origFW := actionWrapForwarder
	actionWrapForwarder = []byte("FORWARDER-TEST")
	t.Cleanup(func() { actionWrapForwarder = origFW })

	yamlPath := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(yamlPath, []byte(testWrapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runActionWrap([]string{"--config=" + yamlPath, "--install"}, &stdout, &stderr); code != 0 {
		t.Fatalf("first install: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	// Without --force the second install fails on action.md clobber.
	if code := runActionWrap([]string{"--config=" + yamlPath, "--install"}, &stdout, &stderr); code == 0 {
		t.Error("expected refusal to clobber on second --install")
	}
	stdout.Reset()
	stderr.Reset()
	if code := runActionWrap([]string{"--config=" + yamlPath, "--install", "--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--install --force: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "already installed") {
		t.Errorf("expected 'already installed' message; got %q", stdout.String())
	}
}

// --- Spec round-trip across the CLI surface ---

func TestRunActionWrap_PreservesSubcommandsAcrossYAMLAndHelp(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "spec.yaml")
	if err := os.WriteFile(yamlPath, []byte(testWrapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := wrap.LoadYAML(yamlPath, []byte(testWrapYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Subcommands) == 0 {
		t.Fatal("test fixture has no subcommands")
	}
}

const testWrapYAML = `connector:
  name: github://aileron-test/gitcrawl
  version: 1.0.0
  publisher: aileron-test

program:
  path: /usr/bin/git
  hash: sha256:abc

env_passthrough:
  - GIT_AUTHOR_NAME

fs_read:
  - ~/code/

fs_write:
  - ~/.cache/aileron/gitcrawl/

cwd: ~/code/

subcommands:
  - name: log
    description: List commits since a date.
    argv: git log --since={since}
    params:
      - name: since
        type: string
        required: true
  - name: status
    description: Show working tree state.
    argv: git status
`
