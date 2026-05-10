package wrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

const goodYAML = `connector:
  name: github://aileron-test/gitcrawl
  version: 1.0.0
  publisher: aileron-test

program:
  path: /usr/bin/git
  hash: sha256:abc123

env_passthrough:
  - GIT_AUTHOR_NAME
  - GH_TOKEN

credential:
  kind: api_key
  scope: read GitHub events on the user's behalf

credential_env_keys:
  - GH_TOKEN

fs_read:
  - ~/code/

fs_write:
  - ~/.cache/aileron/gitcrawl/

cwd: ~/code/

subcommands:
  - name: log
    description: List commits since the given date.
    argv: git log --since={since} --author={author}
    params:
      - name: since
        type: string
        description: ISO-8601 date.
        required: true
      - name: author
        type: string
        description: Email or username.
  - name: status
    description: Show working tree state.
    argv: git status
`

func TestLoadYAML_AcceptsCanonicalForm(t *testing.T) {
	s, err := LoadYAML("ok.yaml", []byte(goodYAML))
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if s.Connector.Name != "github://aileron-test/gitcrawl" {
		t.Errorf("Name = %q", s.Connector.Name)
	}
	if s.Program.Path != "/usr/bin/git" {
		t.Errorf("Program.Path = %q", s.Program.Path)
	}
	if len(s.Subcommands) != 2 {
		t.Errorf("Subcommands len = %d", len(s.Subcommands))
	}
	if s.Subcommands[0].Argv != "git log --since={since} --author={author}" {
		t.Errorf("Subcommands[0].Argv = %q", s.Subcommands[0].Argv)
	}
}

func TestLoadYAML_RejectsBadShapes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"missing name", func(s string) string {
			return strings.Replace(s, "  name: github://aileron-test/gitcrawl\n", "", 1)
		}, "connector.name is required"},
		{"missing version", func(s string) string {
			return strings.Replace(s, "  version: 1.0.0\n", "", 1)
		}, "connector.version is required"},
		{"relative program path", func(s string) string {
			return strings.Replace(s, "/usr/bin/git", "git", 1)
		}, "absolute"},
		{"missing subcommands", func(s string) string {
			i := strings.Index(s, "subcommands:")
			return s[:i]
		}, "at least one subcommand"},
		{"missing argv", func(s string) string {
			return strings.Replace(s, "    argv: git status\n", "", 1)
		}, "argv is required"},
		{"invalid env key", func(s string) string {
			return strings.Replace(s, "  - GH_TOKEN\n", "  - 1BAD\n", 1)
		}, "valid env name"},
		{"credential env not in passthrough", func(s string) string {
			return strings.Replace(s, "credential_env_keys:\n  - GH_TOKEN", "credential_env_keys:\n  - OTHER_TOKEN", 1)
		}, "not in env_passthrough"},
		{"relative cwd", func(s string) string {
			return strings.Replace(s, "cwd: ~/code/", "cwd: code/", 1)
		}, "absolute"},
		{"relative fs_read", func(s string) string {
			return strings.Replace(s, "  - ~/code/", "  - code/", 1)
		}, "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.mutate(goodYAML)
			_, err := LoadYAML("x.yaml", []byte(body))
			if err == nil {
				t.Fatalf("LoadYAML accepted; want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadYAML_RejectsMalformedYAML(t *testing.T) {
	_, err := LoadYAML("bad.yaml", []byte("connector:\n  name: [unbalanced"))
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// --- --help parsing ---

func TestFromHelp_ParsesCobraStyleSubcommandList(t *testing.T) {
	help := `gh works with GitHub from the terminal.

Available Commands:
  pr          Manage pull requests
  issue       Manage issues
  repo        Manage repositories
  auth        Authenticate gh

Flags:
  -h, --help     Show help
  -v, --version  Show version
`
	s, err := FromHelp(context.Background(), staticRunner(help, nil),
		"github://aileron-test/gh", "1.0.0", "/usr/bin/gh")
	if err != nil {
		t.Fatalf("FromHelp: %v", err)
	}
	got := subcommandNames(s.Subcommands)
	want := []string{"pr", "issue", "repo", "auth"}
	if !slicesEqual(got, want) {
		t.Errorf("subcommands = %v, want %v", got, want)
	}
}

func TestFromHelp_ParsesUrfaveStyleHeader(t *testing.T) {
	help := `NAME:
   slackdump - Export Slack workspaces

USAGE:
   slackdump [global options] command [arguments...]

COMMANDS:
   export    Export channels and DMs
   list      List available resources
   help, h   Show help

GLOBAL OPTIONS:
   --help, -h     show help
`
	s, err := FromHelp(context.Background(), staticRunner(help, nil),
		"github://aileron-test/slackdump", "1.0.0", "/usr/local/bin/slackdump")
	if err != nil {
		t.Fatalf("FromHelp: %v", err)
	}
	got := subcommandNames(s.Subcommands)
	if !contains(got, "export") || !contains(got, "list") {
		t.Errorf("subcommands = %v, want export and list", got)
	}
}

func TestFromHelp_NoSubcommandsFallsBackToBareInvocation(t *testing.T) {
	help := `cat reads files and writes them to stdout.

Usage: cat [OPTION]... [FILE]...

  -A, --show-all
  -b, --number-nonblank
`
	s, err := FromHelp(context.Background(), staticRunner(help, nil),
		"github://aileron-test/cat", "1.0.0", "/bin/cat")
	if err != nil {
		t.Fatalf("FromHelp: %v", err)
	}
	if len(s.Subcommands) != 1 || s.Subcommands[0].Name != "run" {
		t.Errorf("subcommands = %+v, want fallback `run`", s.Subcommands)
	}
}

func TestFromHelp_NonZeroExitWithOutputStillParses(t *testing.T) {
	// curl exits non-zero on `--help` in some versions but still
	// produces help on stdout; the parser tolerates that.
	help := `Available Commands:
  one    First subcommand
  two    Second subcommand
`
	s, err := FromHelp(context.Background(), staticRunner(help, errors.New("non-zero")),
		"github://aileron-test/curl", "1.0.0", "/usr/bin/curl")
	if err != nil {
		t.Fatalf("FromHelp: %v", err)
	}
	if len(s.Subcommands) != 2 {
		t.Errorf("subcommands = %+v", s.Subcommands)
	}
}

func TestFromHelp_TrueErrorsBubbleUp(t *testing.T) {
	_, err := FromHelp(context.Background(), staticRunner("", errors.New("boom")),
		"github://aileron-test/x", "1.0.0", "/usr/bin/x")
	if err == nil {
		t.Fatal("expected error to bubble up when help output is empty")
	}
}

func TestFromHelp_RejectsRelativePath(t *testing.T) {
	_, err := FromHelp(context.Background(), staticRunner("", nil),
		"github://aileron-test/x", "1.0.0", "x")
	if err == nil {
		t.Fatal("expected error for relative program path")
	}
}

// --- Emit / BuildManifest end-to-end ---

func TestEmit_ProducesValidManifest(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadYAML("a.yaml", []byte(goodYAML))
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if err := Emit(s, dir, false); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want := []string{
		"connector/manifest.toml",
		"actions/log/action.md",
		"actions/status/action.md",
		"Taskfile.yml",
		".github/workflows/release.yml",
		"keys/README.md",
	}
	for _, p := range want {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(dir, "connector/manifest.toml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := cstore.ParseManifest("manifest.toml", body)
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, body)
	}
	if err := cstore.ValidateManifest(m, "manifest.toml"); err != nil {
		t.Errorf("emitted manifest does not validate: %v", err)
	}
	if m.Capabilities.Spawn == nil {
		t.Fatal("manifest is missing [capabilities.spawn]")
	}
	if got, want := len(m.Capabilities.Spawn.ArgvPatterns), 2; got != want {
		t.Errorf("argv_patterns count = %d, want %d", got, want)
	}
}

func TestEmit_RefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	if err := Emit(s, dir, false); err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	err := Emit(s, dir, false)
	if err == nil {
		t.Fatal("expected refusal on second emit without force")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("err = %v", err)
	}
}

func TestEmit_ForceReplacesExistingTree(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	if err := Emit(s, dir, false); err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	if err := Emit(s, dir, true); err != nil {
		t.Fatalf("Emit(force): %v", err)
	}
}

func TestEmit_FailsLoudlyOnInvalidSpec(t *testing.T) {
	// A Spec that bypassed the loader's validate() must still fail
	// before any file is written. Construct one inline.
	s := &Spec{
		Connector:   ConnectorSpec{Name: "github://x/y", Version: "0.0.1"},
		Program:     ProgramSpec{Path: "git"}, // relative, bypasses validate
		Subcommands: []SubcommandSpec{{Name: "x", Argv: "git x"}},
	}
	dir := t.TempDir()
	err := Emit(s, dir, false)
	if err == nil {
		t.Fatal("expected validation failure for relative program path")
	}
}

func TestBuildManifest_PreservesSpawnFields(t *testing.T) {
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	m := BuildManifest(s)
	sp := m.Capabilities.Spawn
	if sp == nil {
		t.Fatal("Capabilities.Spawn nil")
	}
	if got := sp.Programs[0].Hash; got != "sha256:abc123" {
		t.Errorf("Hash = %q", got)
	}
	if !contains(sp.EnvPassthrough, "GH_TOKEN") {
		t.Errorf("EnvPassthrough = %v", sp.EnvPassthrough)
	}
	if sp.Cwd != "~/code/" {
		t.Errorf("Cwd = %q", sp.Cwd)
	}
}

func TestSortedSubcommands_ReturnsAlphabeticalOrder(t *testing.T) {
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	got := SortedSubcommands(s)
	if got[0].Name != "log" || got[1].Name != "status" {
		t.Errorf("SortedSubcommands = %+v", got)
	}
}

// --- helpers ---

// staticRunner returns a HelpRunner that always yields the supplied
// output and error.
func staticRunner(out string, err error) HelpRunner {
	return func(_ context.Context, _ string, _ []string) (string, error) {
		return out, err
	}
}

func subcommandNames(subs []SubcommandSpec) []string {
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = s.Name
	}
	return out
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}
