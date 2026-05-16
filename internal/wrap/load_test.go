package wrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
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
	rootHelp := `gh works with GitHub from the terminal.

Available Commands:
  pr          Manage pull requests
  issue       Manage issues
  repo        Manage repositories
  auth        Authenticate gh

Flags:
  -h, --help     Show help
  -v, --version  Show version
`
	// Each top-level subcommand resolves to a leaf with no further
	// nesting in this fixture, so the recursion bottoms out
	// immediately and emits one operation per top-level name —
	// matching the v1 contract before the recursive walk landed.
	leafHelp := `Manage things.

Usage:
  thing <subject>
`
	runner := pathRunner(map[string]string{
		"":      rootHelp,
		"pr":    leafHelp,
		"issue": leafHelp,
		"repo":  leafHelp,
		"auth":  leafHelp,
	})
	s, err := FromHelp(context.Background(), runner,
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

// TestFromHelp_RecursesIntoNamespacesAndEmitsLeafOperations is the
// new-contract test: when a top-level subcommand is a namespace
// (has its own Available Commands block), the walk recurses and
// emits one operation per leaf — joined-by-dash for the op name.
func TestFromHelp_RecursesIntoNamespacesAndEmitsLeafOperations(t *testing.T) {
	rootHelp := `linear-pp-cli — Linear from the terminal.

Available Commands:
  issues      Get, list, or create Linear issues
  teams       Manage teams
`
	issuesHelp := `Linear issue operations.

Available Commands:
  create      Create a new issue
  list        List issues
`
	createHelp := `Create a new Linear issue.

Flags:
      --title string         Issue title (required)
      --team string          Team key (required)
      --description string   Issue description (markdown)
      --priority int         Priority: 1=Urgent, 2=High
`
	listHelp := `List Linear issues.

Flags:
      --assignee string   Assignee user UUID
      --state string      Workflow state UUID
`
	teamsHelp := `Manage teams.

Usage:
  linear-pp-cli teams [flags]
`
	runner := pathRunner(map[string]string{
		"":              rootHelp,
		"issues":        issuesHelp,
		"issues create": createHelp,
		"issues list":   listHelp,
		"teams":         teamsHelp,
	})
	s, err := FromHelp(context.Background(), runner,
		"github://aileron-test/linear-pp-cli", "1.0.0", "/usr/local/bin/linear-pp-cli")
	if err != nil {
		t.Fatalf("FromHelp: %v", err)
	}
	got := subcommandNames(s.Subcommands)
	want := []string{"issues-create", "issues-list", "teams"}
	if !slicesEqual(got, want) {
		t.Fatalf("subcommands = %v, want %v", got, want)
	}
	// issues-create should carry the four parsed flags with title +
	// team marked required, description + priority optional. Order
	// follows required-first then optional, preserving Cobra's
	// alphabetized listing within each group.
	var create *SubcommandSpec
	for i := range s.Subcommands {
		if s.Subcommands[i].Name == "issues-create" {
			create = &s.Subcommands[i]
			break
		}
	}
	if create == nil {
		t.Fatal("issues-create not found")
	}
	wantParams := []ParamSpec{
		{Name: "title", Type: "string", Required: true},
		{Name: "team", Type: "string", Required: true},
		{Name: "description", Type: "string"},
		{Name: "priority", Type: "integer"},
	}
	if len(create.Params) != len(wantParams) {
		t.Fatalf("issues-create params: got %d want %d (%+v)", len(create.Params), len(wantParams), create.Params)
	}
	for i, p := range create.Params {
		if p.Name != wantParams[i].Name || p.Type != wantParams[i].Type || p.Required != wantParams[i].Required {
			t.Errorf("param[%d]: got %+v, want name=%s type=%s required=%v",
				i, p, wantParams[i].Name, wantParams[i].Type, wantParams[i].Required)
		}
	}
	// argv should be `"issues create --title {title} --team {team} [--description {description}] [--priority {priority}]"`.
	wantArgv := "issues create --title {title} --team {team} [--description {description}] [--priority {priority}]"
	if create.Argv != wantArgv {
		t.Errorf("argv = %q\n  want = %q", create.Argv, wantArgv)
	}
}

// TestBuildLeafSpec_TranslatesDashedFlagNamesToUnderscoreInputs pins the
// load-bearing regression: Cobra long-form flag names with dashes
// (`--data-source`, `--group-by`, `--auth-set-api-key`) must translate
// to underscore form for the action-input `name` field and the
// argv-pattern placeholder, since the action-manifest validator enforces
// `^[a-z][a-z0-9_]*$` on input names. The argv *literal* keeps the
// original dashed spelling — that's what the wrapped CLI parses.
//
// Without this translation, every leaf with a dashed flag would
// silently fail to load (validation_error) at daemon startup and the
// agent's tool catalog would be missing the action — the exact failure
// mode that bit `aileron pp add linear` after #797 (linear-pp-cli's
// 69 leaves carry globals like --data-source on every command).
func TestBuildLeafSpec_TranslatesDashedFlagNamesToUnderscoreInputs(t *testing.T) {
	rootHelp := `linear-pp-cli — Linear from the terminal.

Available Commands:
  cycles      Cycle operations
`
	leafHelp := `Compare cycles.

Flags:
      --data-source string   Data source: live, local, auto
      --group-by string      Field to group by (required)
`
	runner := pathRunner(map[string]string{
		"":               rootHelp,
		"cycles":         leafHelp,
	})
	s, err := FromHelp(context.Background(), runner,
		"github://aileron-test/linear-pp-cli", "1.0.0", "/usr/local/bin/linear-pp-cli")
	if err != nil {
		t.Fatalf("FromHelp: %v", err)
	}
	if len(s.Subcommands) != 1 {
		t.Fatalf("subcommands = %d, want 1", len(s.Subcommands))
	}
	sub := s.Subcommands[0]
	// Input names must be underscore-converted.
	wantNames := map[string]bool{"group_by": true, "data_source": true}
	for _, p := range sub.Params {
		if !wantNames[p.Name] {
			t.Errorf("param name = %q; want one of %v (dash-to-underscore translation regressed)", p.Name, wantNames)
		}
	}
	// argv literal keeps the dashed spelling; placeholder uses underscores.
	wantArgv := "cycles --group-by {group_by} [--data-source {data_source}]"
	if sub.Argv != wantArgv {
		t.Errorf("argv = %q\n  want = %q", sub.Argv, wantArgv)
	}
}

// TestBuildLeafSpec_PreservesUnderscoreFlagNames covers the no-op path
// — flag names that already use underscores (rare in Cobra but possible
// elsewhere) pass through unchanged in both the input name and argv.
func TestBuildLeafSpec_PreservesUnderscoreFlagNames(t *testing.T) {
	leaf := buildLeafSpec(
		[]string{"do"},
		"Do a thing.",
		[]flagSpec{
			{Name: "snake_case_flag", Type: "string", Required: true},
		},
	)
	if leaf.Params[0].Name != "snake_case_flag" {
		t.Errorf("Name = %q, want snake_case_flag", leaf.Params[0].Name)
	}
	if leaf.Argv != "do --snake_case_flag {snake_case_flag}" {
		t.Errorf("argv = %q", leaf.Argv)
	}
}

// TestBuildLeafSpec_OutputValidatesAgainstActionManifest is the
// belt-and-suspenders check: a Spec emitted from a realistic Cobra
// leaf (dashed flags, mix of required/optional) must produce an
// action.md that round-trips through internal/action.Validate without
// surfacing any inputName regex failures. Catches the kind of
// validation-error miss this PR fixes.
func TestBuildLeafSpec_OutputValidatesAgainstActionManifest(t *testing.T) {
	// End-to-end: build a Spec with dashed Cobra flag names, render
	// it through the production emit path (RenderActionMD), parse
	// it back via action.Parse, and run action.Validate against the
	// result. This is the regression boundary for the dash-bug —
	// before this PR, action.Validate rejected every leaf with a
	// dashed flag (data-source, group-by, auth-set-api-key, ...)
	// and the daemon silently dropped all 83 emitted linear-* files
	// from its action store at load time.
	leaf := buildLeafSpec(
		[]string{"issues", "create"},
		"Create a Linear issue",
		[]flagSpec{
			{Name: "title", Type: "string", Required: true},
			{Name: "team", Type: "string", Required: true},
			{Name: "data-source", Type: "string", Required: false},
			{Name: "auth-set-api-key", Type: "string", Required: false},
			{Name: "group-by", Type: "string", Required: false},
		},
	)
	inputNameRe := `^[a-z][a-z0-9_]*$`
	matcher := regexp.MustCompile(inputNameRe)
	for _, p := range leaf.Params {
		if !matcher.MatchString(p.Name) {
			t.Errorf("param name %q does not match %s — would fail action.Validate", p.Name, inputNameRe)
		}
	}

	spec := &Spec{
		Connector:   ConnectorSpec{Name: "local://user/linear", Version: "0.0.1"},
		Program:     ProgramSpec{Path: "/usr/local/bin/linear-pp-cli"},
		Subcommands: []SubcommandSpec{leaf},
	}
	manifest := BuildManifest(spec)
	md := RenderActionMD(spec, leaf, manifest, "sha256:bound-at-install")
	parsed, err := action.Parse(ActionFileName(spec, leaf), []byte(md))
	if err != nil {
		t.Fatalf("emitted action.md does not parse: %v\n%s", err, md)
	}
	if err := action.Validate(parsed, ActionFileName(spec, leaf)); err != nil {
		t.Fatalf("emitted action.md fails validation: %v\n%s", err, md)
	}
}

func TestFromHelp_ParsesUrfaveStyleHeader(t *testing.T) {
	rootHelp := `NAME:
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
	leafHelp := `Do the thing.

Usage:
  slackdump <action>
`
	runner := pathRunner(map[string]string{
		"":       rootHelp,
		"export": leafHelp,
		"list":   leafHelp,
		// "help" is filtered by parseSubcommands's name regex
		// (lowercase + alphanumeric + dash); "help, h" was already
		// dropped before the recursion, so we don't fixture it.
	})
	s, err := FromHelp(context.Background(), runner,
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
	rootHelp := `Available Commands:
  one    First subcommand
  two    Second subcommand
`
	leafHelp := `Some leaf.

Usage:
  curl <thing>
`
	// pathRunner returns no error for known paths; the non-zero-exit
	// case is exercised by wrapping the root response in a runner
	// that yields output AND an error. We need the root invocation
	// to error but the child invocations to succeed, so build a
	// composite manually.
	runner := func(_ context.Context, _ string, args []string) (string, error) {
		if len(args) == 1 && args[0] == "--help" {
			return rootHelp, errors.New("non-zero")
		}
		return leafHelp, nil
	}
	s, err := FromHelp(context.Background(), runner,
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
		"actions/log.md",
		"actions/status.md",
	}
	for _, p := range want {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}
	// Default output is slim: no Taskfile/release.yml/keys
	// — shared-forwarder connectors don't build their own WASM.
	for _, p := range []string{"Taskfile.yml", ".github/workflows/release.yml", "keys/README.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("default output should not include %s", p)
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
	if got, want := len(m.Capabilities.Spawn.Operations), 2; got != want {
		t.Errorf("operations count = %d, want %d", got, want)
	}
}

func TestEmit_ActionMDIsParseableByActionLoader(t *testing.T) {
	// The daemon's internal/action loader expects +++-delimited TOML
	// frontmatter. Earlier versions of wrap emitted ---/YAML, which
	// the loader rejected silently. This is the regression test.
	dir := t.TempDir()
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	if err := Emit(s, dir, false); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "actions/log.md"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, parseErr := action.Parse("log.md", body)
	if parseErr != nil {
		t.Fatalf("emitted action.md does not parse: %v\n%s", parseErr, body)
	}
	// Per #781 the rendered name is namespaced by the connector
	// leaf so two wraps with overlapping op names don't collide
	// in the action store's name-keyed map.
	if manifest.Name != "gitcrawl-log" {
		t.Errorf("manifest.Name = %q, want gitcrawl-log", manifest.Name)
	}
	if got := len(manifest.Requires.Connectors); got != 1 {
		t.Fatalf("requires.connectors len = %d, want 1", got)
	}
	if got := manifest.Requires.Connectors[0].Name; got != "github://aileron-test/gitcrawl" {
		t.Errorf("connector ref name = %q", got)
	}
	if got := manifest.Requires.Connectors[0].Hash; got != "sha256:bound-at-install" {
		t.Errorf("connector ref hash = %q, want placeholder", got)
	}
	if len(manifest.Execute) != 1 || manifest.Execute[0].Op != "log" {
		t.Errorf("execute = %+v", manifest.Execute)
	}
}

func TestEmit_ActionMDInputsLandWhenSpecDeclaresParams(t *testing.T) {
	// The "log" subcommand in goodYAML has params (since, author);
	// the emitted action.md surfaces them as [[inputs]] entries so
	// the LLM tool catalog sees the parameter shape. The wrap
	// emitter deliberately does NOT emit [execute.inputs] — the
	// agent's call-time args flow directly to the connector through
	// mergeArgs (the input names match the argv-pattern placeholder
	// names by construction), and emitting `${args.X}` here would
	// break optional-group elision for unsupplied inputs by leaving
	// the literal token in the merged args map.
	dir := t.TempDir()
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	if err := Emit(s, dir, false); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "actions/log.md"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := action.Parse("log.md", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Inputs) == 0 {
		t.Fatal("expected [[inputs]] entries from spec params")
	}
	// [execute.inputs] must be empty/absent — wrap-emitted actions
	// rely on the args-iteration path in mergeArgs to deliver the
	// agent's inputs to the connector.
	if len(manifest.Execute) != 1 {
		t.Fatalf("expected one execute step, got %d", len(manifest.Execute))
	}
	if len(manifest.Execute[0].Inputs) != 0 {
		t.Errorf("execute.inputs should be empty in wrap-emitted action.md; got %+v",
			manifest.Execute[0].Inputs)
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

// --- Install (write to daemon store + actions dir) ---

func TestInstall_WritesManifestToStoreAndActionFiles(t *testing.T) {
	s, err := LoadYAML("a.yaml", []byte(goodYAML))
	if err != nil {
		t.Fatal(err)
	}
	storeRoot := t.TempDir()
	actionsDir := t.TempDir()
	store := cstore.NewStore(storeRoot)
	forwarderBytes := []byte("FORWARDER-TEST")

	res, err := Install(s, store, actionsDir, forwarderBytes, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Hash == "" || !strings.HasPrefix(res.Hash, "sha256:") {
		t.Errorf("Hash = %q, want sha256:<hex>", res.Hash)
	}
	if res.AlreadyInstalled {
		t.Error("first install reported AlreadyInstalled")
	}
	if _, err := os.Stat(filepath.Join(res.EntryDir, "manifest.toml")); err != nil {
		t.Errorf("store manifest missing: %v", err)
	}
	if len(res.ActionFiles) != len(s.Subcommands) {
		t.Errorf("ActionFiles len = %d, want %d", len(res.ActionFiles), len(s.Subcommands))
	}
	// Action files use a connector-leaf prefix so multiple wraps with
	// the same op name don't collide.
	wantNames := []string{"gitcrawl-log.md", "gitcrawl-status.md"}
	for _, name := range wantNames {
		if _, err := os.Stat(filepath.Join(actionsDir, name)); err != nil {
			t.Errorf("expected %s in actions dir: %v", name, err)
		}
	}
	// And those action files parse via the daemon's loader.
	body, err := os.ReadFile(filepath.Join(actionsDir, "gitcrawl-log.md"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := action.Parse("gitcrawl-log.md", body)
	if err != nil {
		t.Fatalf("installed action.md does not parse: %v", err)
	}
	// In --install mode the connector hash is baked in pre-bound,
	// not the bound-at-install placeholder.
	if m.Requires.Connectors[0].Hash != res.Hash {
		t.Errorf("connector hash in action.md = %q, want store hash %q",
			m.Requires.Connectors[0].Hash, res.Hash)
	}
}

func TestInstall_ReinstallSameSpecIsIdempotent(t *testing.T) {
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	store := cstore.NewStore(t.TempDir())
	actionsDir := t.TempDir()
	fw := []byte("FW")
	first, err := Install(s, store, actionsDir, fw, false)
	if err != nil {
		t.Fatal(err)
	}
	// Second install with force=true (we already wrote the action.md files).
	second, err := Install(s, store, actionsDir, fw, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash {
		t.Errorf("hashes diverged: %q vs %q", first.Hash, second.Hash)
	}
	if !second.AlreadyInstalled {
		t.Error("second install should report AlreadyInstalled")
	}
}

func TestInstall_RefusesToClobberActionFilesWithoutForce(t *testing.T) {
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	store := cstore.NewStore(t.TempDir())
	actionsDir := t.TempDir()
	if _, err := Install(s, store, actionsDir, []byte("FW"), false); err != nil {
		t.Fatal(err)
	}
	_, err := Install(s, store, actionsDir, []byte("FW"), false)
	if err == nil {
		t.Fatal("expected refusal on second install without force")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("err = %v", err)
	}
}

func TestInstall_RejectsBadConnectorFQN(t *testing.T) {
	s, _ := LoadYAML("a.yaml", []byte(goodYAML))
	s.Connector.Name = "not-a-valid-fqn" // bypass LoadYAML's earlier validation
	store := cstore.NewStore(t.TempDir())
	_, err := Install(s, store, t.TempDir(), []byte("FW"), false)
	if err == nil {
		t.Fatal("expected error for unparseable FQN")
	}
}

func TestActionFileName_DerivedFromConnectorLeaf(t *testing.T) {
	cases := []struct {
		fqn  string
		op   string
		want string
	}{
		{"github://acme/gitcrawl", "log", "gitcrawl-log.md"},
		{"github://acme/integrations/connectors/imsg", "send", "imsg-send.md"},
		// No slash → no leaf segment to derive; the file lands as
		// "<op>.md" without a prefix. In practice the FQN is parsed
		// before this is reached, so this case is mostly defensive.
		{"weird-no-slash", "op", "op.md"},
	}
	for _, c := range cases {
		s := &Spec{Connector: ConnectorSpec{Name: c.fqn}}
		got := actionFileName(s, SubcommandSpec{Name: c.op})
		if got != c.want {
			t.Errorf("actionFileName(%q, %q) = %q, want %q", c.fqn, c.op, got, c.want)
		}
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
//
// Tests that exercise the recursive --help walk should prefer
// [pathRunner] instead — staticRunner returns the same body for
// every subcommand path, so a multi-level walk against it would
// either hit the depth cap or emit deeply-nested leaf names that
// don't reflect a real CLI. staticRunner is still appropriate for
// single-level tests (the CLI being wrapped has no nested verbs).
func staticRunner(out string, err error) HelpRunner {
	return func(_ context.Context, _ string, _ []string) (string, error) {
		return out, err
	}
}

// pathRunner returns a HelpRunner that yields different output per
// subcommand path. The key is the space-separated verb path (`""`
// for the bare program, `"pr"` for `<program> pr`, `"pr create"`
// for `<program> pr create`), and the value is the body the
// runner returns when `<program> <key> --help` is invoked.
//
// Paths absent from the map return an empty string + a synthetic
// "no help" error — same shape a real CLI exit with no output
// produces. The wrap walker treats that as a failed subcommand
// and drops it from emission, mirroring production behavior.
func pathRunner(byPath map[string]string) HelpRunner {
	return func(_ context.Context, _ string, args []string) (string, error) {
		// args carries `<path...> --help` — drop the trailing
		// `--help` to build the key.
		key := ""
		if n := len(args); n > 0 && args[n-1] == "--help" {
			key = strings.Join(args[:n-1], " ")
		} else {
			key = strings.Join(args, " ")
		}
		body, ok := byPath[key]
		if !ok {
			return "", errors.New("no help for path " + key)
		}
		return body, nil
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
