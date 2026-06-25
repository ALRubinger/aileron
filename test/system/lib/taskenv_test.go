// Regression contract for the env/var wiring of the run-by-hand launch
// scenarios and the host-arch build outputs, ported from the POSIX-shell
// taskenv_test.sh. Pure Go + the `task` and `go` binaries the lib tier already
// runs under; no Docker and no `aileron launch`, so it is CI-safe and
// unattended.
//
// The shell test split into two halves: STRUCTURAL checks (the real
// Taskfile.yml keeps env/vars in the shape the fix depends on) and BEHAVIORAL
// checks (go-task's runtime honors that shape). This port keeps the same split:
//
//   - Structural checks parse Taskfile.yml with gopkg.in/yaml.v3 and assert on
//     the parsed task/var/cmd tree, so they survive reformatting (comment edits,
//     indentation changes) the way the shell's awk/indent matching could not.
//     Where an invariant is inherently about raw template text (a `-o build/...`
//     output, the `now | unixEpoch` funcs, forbidden `date +%s`/`mktemp`/`$$`/
//     `/tmp` tokens), the assertion is scoped to the relevant cmd/var scalar
//     node from the YAML model rather than a flat file grep, so it is immune to
//     comment text.
//
//   - Behavioral checks drive the real `task` binary against tiny Taskfiles
//     written to t.TempDir(), with a Go-controlled cmd.Env/PATH. No symlinks
//     (repo-forbidden); the "date/mktemp off PATH" sandbox is a Go-created temp
//     dir holding only the resolved `task`/`sh` binaries. The probe is a
//     pure-template `echo` cmd (no `sh probe.sh`) so the positive/negative env
//     cases run on Windows too.
//
// No-silent-skip: if `task` or `go` is not resolvable on PATH the test calls
// t.Fatal (loud failure), matching the shell test's `exit 1`. Never t.Skip.
package systestlib_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Binary resolution (fail loud, never skip)
// ---------------------------------------------------------------------------

// taskBin resolves the `task` binary once, failing loud if it is absent. The
// shell test exits 1 when `task` is off PATH; here the equivalent is t.Fatal.
func taskBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("task")
	if err != nil {
		t.Fatalf("`task` binary not on PATH (required to validate env wiring): %v", err)
	}
	return p
}

// goBin resolves the `go` binary once, failing loud if it is absent. `go` is a
// hard prerequisite for HOST_EXE resolution; the shell test reported its
// absence as a real failure rather than a skip.
func goBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("`go` binary not on PATH (required to resolve HOST_EXE behaviorally): %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Taskfile locator + YAML model
// ---------------------------------------------------------------------------

// taskfilePath computes the repo-root Taskfile.yml path the same way the shell
// test did: three levels up from this package (test/system/lib -> repo root).
// runtime.Caller is the robust locator because `go test` runs with cwd = the
// package dir, and this package is its own module rooted at test/system/lib, so
// neither cwd nor {{.ROOT_DIR}} can be assumed.
func taskfilePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate this test file")
	}
	// thisFile = <repo>/test/system/lib/taskenv_test.go
	libDir := filepath.Dir(thisFile)
	root := filepath.Clean(filepath.Join(libDir, "..", "..", ".."))
	tf := filepath.Join(root, "Taskfile.yml")
	if _, err := os.Stat(tf); err != nil {
		t.Fatalf("repo-root Taskfile.yml not readable at %s: %v", tf, err)
	}
	return tf
}

// loadTaskfile reads and parses Taskfile.yml into a yaml.Node tree once. Both
// the structured task->env/vars maps and the raw scalar strings (cmd lines,
// var `sh:` bodies) are reachable from the node tree.
func loadTaskfile(t *testing.T) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(taskfilePath(t))
	if err != nil {
		t.Fatalf("reading Taskfile.yml: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing Taskfile.yml: %v", err)
	}
	// A document node wraps the root mapping; unwrap to the mapping node.
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0]
	}
	return &doc
}

// mapGet returns the value node for `key` in a YAML mapping node, or nil if the
// node is not a mapping or the key is absent. Mapping content alternates
// key,value,key,value...
func mapGet(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// mapKeys returns the ordered key names of a YAML mapping node.
func mapKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys = append(keys, node.Content[i].Value)
	}
	return keys
}

// tasksNode returns the top-level `tasks:` mapping node.
func tasksNode(t *testing.T, root *yaml.Node) *yaml.Node {
	t.Helper()
	tn := mapGet(root, "tasks")
	if tn == nil || tn.Kind != yaml.MappingNode {
		t.Fatal("Taskfile.yml has no `tasks:` mapping")
	}
	return tn
}

// taskNode returns the named task's mapping node, failing loud if absent (the
// shell test treated a missing target block as a failure, not a skip).
func taskNode(t *testing.T, root *yaml.Node, name string) *yaml.Node {
	t.Helper()
	tn := mapGet(tasksNode(t, root), name)
	if tn == nil {
		t.Fatalf("task %q not found in Taskfile.yml", name)
	}
	return tn
}

// scalarValue extracts the effective string of a var/env value node. A value is
// either a plain scalar (`KEY: literal`) or a mapping carrying a dynamic source
// (`KEY:` / `sh: <cmd>`). For the mapping form it returns the `sh:` body so the
// forbidden-token scan and the `go env GOEXE` check see the command text.
func scalarValue(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value
	}
	if node.Kind == yaml.MappingNode {
		if sh := mapGet(node, "sh"); sh != nil && sh.Kind == yaml.ScalarNode {
			return sh.Value
		}
	}
	return ""
}

// collectScalars walks a node subtree and appends every scalar Value, so an
// invariant about "any cmd/template string under this task" can scan the whole
// subtree without hand-coding every shape (cmds may be strings, `cmd:` maps,
// `task:`/`defer:` maps, nested vars, etc.).
func collectScalars(node *yaml.Node, out *[]string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		*out = append(*out, node.Value)
		return
	}
	for _, c := range node.Content {
		collectScalars(c, out)
	}
}

// allScalars returns every scalar string anywhere under node.
func allScalars(node *yaml.Node) []string {
	var out []string
	collectScalars(node, &out)
	return out
}

// ---------------------------------------------------------------------------
// (B) launch targets keep AILERON_BIN et al. at task scope
// ---------------------------------------------------------------------------

// TestTaskfileLaunchEnvAtTaskScope pins half B of the #1485 contract: the
// run-by-hand launch targets declare AILERON_BIN, AILERON_SYSTEST_LIB and
// WORKSPACE under the TASK-level `env:` (which go-task honors) and never under a
// `cmds:` entry's `env:` (which go-task silently drops). Asserted against the
// parsed task tree, so moving an env key back under a command fails here.
func TestTaskfileLaunchEnvAtTaskScope(t *testing.T) {
	root := loadTaskfile(t)
	requiredKeys := []string{"AILERON_BIN", "AILERON_SYSTEST_LIB", "WORKSPACE"}
	targets := []string{"test:system:launch:codex", "test:system:launch:claude"}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			task := taskNode(t, root, target)
			envNode := mapGet(task, "env")
			if envNode == nil || envNode.Kind != yaml.MappingNode {
				t.Fatalf("%s: no task-level env: mapping", target)
			}
			taskEnvKeys := map[string]bool{}
			for _, k := range mapKeys(envNode) {
				taskEnvKeys[k] = true
			}
			for _, key := range requiredKeys {
				if !taskEnvKeys[key] {
					t.Errorf("%s: %s not declared under task-level env:", target, key)
				}
			}
			// Negative: none of the required keys may appear under any
			// cmds[].env: mapping (the dropped-env footgun).
			cmds := mapGet(task, "cmds")
			if cmds != nil && cmds.Kind == yaml.SequenceNode {
				for _, entry := range cmds.Content {
					cmdEnv := mapGet(entry, "env")
					if cmdEnv == nil {
						continue
					}
					for _, k := range mapKeys(cmdEnv) {
						for _, key := range requiredKeys {
							if k == key {
								t.Errorf("%s: %s declared under a cmds entry env: (go-task would drop it)", target, key)
							}
						}
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (C) SESSION/WORKSPACE vars are Unix-shell-free (#1547)
// ---------------------------------------------------------------------------

// sanctionedTmpFallback is the one allowed `/tmp` literal: the final element of
// the TMPDIR default chain. The shell test stripped this before scanning so a
// genuine hardcoded `/tmp/...` LHS is still caught.
var sanctionedTmpFallback = regexp.MustCompile(`default\s+"/tmp"`)

// forbiddenVarToken matches the non-portable forms go-task's embedded shell
// cannot resolve on Windows: `date +%s`, a `$$` PID, `mktemp`, or any `/tmp`
// literal (after the sanctioned fallback is stripped).
var forbiddenVarToken = regexp.MustCompile(`date \+%s|\$\$|mktemp|/tmp`)

// varDefStrings returns the definition strings for var `key` in task `target`:
// the inline scalar value and/or any `sh:` body, so the scan covers both the
// pure-template inline form and the legacy dynamic form.
func varDefStrings(task *yaml.Node, key string) []string {
	varsNode := mapGet(task, "vars")
	val := mapGet(varsNode, key)
	if val == nil {
		return nil
	}
	var out []string
	if val.Kind == yaml.ScalarNode {
		out = append(out, val.Value)
	}
	if val.Kind == yaml.MappingNode {
		// e.g. KEY: { sh: ... } — collect every scalar under it.
		out = append(out, allScalars(val)...)
	}
	return out
}

// TestTaskfileVarsPortable pins half C of #1547: the three run-by-hand
// system-test targets derive SESSION/WORKSPACE only via portable template funcs,
// never `date +%s`, `$$`, `mktemp`, or a hardcoded `/tmp` (other than the
// sanctioned default fallback).
func TestTaskfileVarsPortable(t *testing.T) {
	root := loadTaskfile(t)
	targets := []string{"test:system:smoke", "test:system:launch:codex", "test:system:launch:claude"}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			task := taskNode(t, root, target)
			for _, key := range []string{"SESSION", "WORKSPACE"} {
				// Pin presence + shape: each target MUST define the var, and as an
				// inline scalar (the portable pure-template form). Without this the
				// forbidden-token scan would pass vacuously if a var were deleted or
				// reverted to a dynamic `sh:` mapping — silently un-pinning the
				// contract this test exists to guard.
				val := mapGet(mapGet(task, "vars"), key)
				if val == nil {
					t.Fatalf("%s: var %s is not defined", target, key)
				}
				if val.Kind != yaml.ScalarNode {
					t.Fatalf("%s: var %s must be an inline scalar (pure-template), got a %v definition", target, key, val.Kind)
				}
				defs := varDefStrings(task, key)
				if len(defs) == 0 {
					t.Fatalf("%s: var %s has no extractable definition", target, key)
				}
				for _, def := range defs {
					scan := sanctionedTmpFallback.ReplaceAllString(def, "")
					if m := forbiddenVarToken.FindString(scan); m != "" {
						t.Errorf("%s: %s definition %q uses non-portable token %q", target, key, def, m)
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (E1) HOST_EXE structural invariants (#1590)
// ---------------------------------------------------------------------------

// hostExeRef matches a {{.HOST_EXE}} template reference.
var hostExeRef = regexp.MustCompile(`\{\{\s*\.HOST_EXE\s*\}\}`)

// hostExeVarNode returns a task's `vars.HOST_EXE` value node, or nil.
func hostExeVarNode(task *yaml.Node) *yaml.Node {
	return mapGet(mapGet(task, "vars"), "HOST_EXE")
}

// TestTaskfileHostExeSourcedFromGoEnv pins E1a: every HOST_EXE var definition in
// the file is `{ sh: go env GOEXE }` — the canonical host primitive. A literal
// or any other source would drift.
func TestTaskfileHostExeSourcedFromGoEnv(t *testing.T) {
	root := loadTaskfile(t)
	tasks := tasksNode(t, root)
	found := 0
	for _, name := range mapKeys(tasks) {
		task := mapGet(tasks, name)
		hx := hostExeVarNode(task)
		if hx == nil {
			continue
		}
		found++
		sh := mapGet(hx, "sh")
		if sh == nil || strings.TrimSpace(sh.Value) != "go env GOEXE" {
			t.Errorf("%s: HOST_EXE must be `sh: go env GOEXE`, got %q", name, scalarValue(hx))
		}
	}
	if found == 0 {
		t.Error("no HOST_EXE var definitions found; expected at least one")
	}
}

// TestTaskfileHostExeNotGlobal pins E1a2: HOST_EXE is NOT a top-level (global)
// var. A global `sh:` var is evaluated eagerly on every task invocation, so a
// global `go env GOEXE` would abort Go-free targets (build:docs, build:webapp)
// on a host without `go`.
func TestTaskfileHostExeNotGlobal(t *testing.T) {
	root := loadTaskfile(t)
	globalVars := mapGet(root, "vars")
	if mapGet(globalVars, "HOST_EXE") != nil {
		t.Error("HOST_EXE must not be a top-level global var (eager go-env would break Go-free tasks)")
	}
}

// TestTaskfileHostExeDefinedWhereReferenced pins E1a3: every task that
// references {{.HOST_EXE}} also defines it in its own vars. An undefined
// reference resolves to the empty string under go-task (no error), silently
// reintroducing the Windows bug.
func TestTaskfileHostExeDefinedWhereReferenced(t *testing.T) {
	root := loadTaskfile(t)
	tasks := tasksNode(t, root)
	for _, name := range mapKeys(tasks) {
		task := mapGet(tasks, name)
		references := false
		for _, s := range allScalars(task) {
			if hostExeRef.MatchString(s) {
				references = true
				break
			}
		}
		if !references {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if hostExeVarNode(task) == nil {
				t.Errorf("%s: references {{.HOST_EXE}} but does not define HOST_EXE in its vars", name)
			}
		})
	}
}

// hostArchOutput matches the host-arch build output that MUST append
// {{.HOST_EXE}}: `-o build/<name>{{.HOST_EXE}}` with a trailing space. Anchoring
// on the suffix avoids cross-matching prefixes (aileron-mcp vs the linux
// sibling, aileron vs aileron-server/-mcp).
func hostArchOutputRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`-o build/` + regexp.QuoteMeta(name) + `\{\{\s*\.HOST_EXE\s*\}\} `)
}

// bareHostArchOutputRe matches a suffix-less host-arch output `-o build/<name> `
// (the pre-fix Windows regression). Negative lookahead is unavailable in RE2, so
// callers strip the with-suffix occurrences first.
func bareHostArchOutputRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`-o build/` + regexp.QuoteMeta(name) + ` `)
}

// TestTaskfileHostArchOutputsAppendHostExe pins E1b: the three host-arch build
// outputs (server/mcp/cli) append {{.HOST_EXE}}, and no suffix-less survivor
// remains. Scanned over the build tasks' cmd scalars from the YAML model, not a
// flat grep, so a comment mentioning `-o build/aileron` cannot satisfy or break
// the check.
func TestTaskfileHostArchOutputsAppendHostExe(t *testing.T) {
	root := loadTaskfile(t)
	tasks := tasksNode(t, root)
	// Collect every cmd scalar across all tasks (build outputs live in cmds).
	var cmdStrings []string
	for _, name := range mapKeys(tasks) {
		cmds := mapGet(mapGet(tasks, name), "cmds")
		cmdStrings = append(cmdStrings, allScalars(cmds)...)
	}
	joined := strings.Join(cmdStrings, "\n")

	specs := []string{"aileron-server", "aileron-mcp", "aileron"}
	for _, spec := range specs {
		t.Run(spec, func(t *testing.T) {
			if !hostArchOutputRe(spec).MatchString(joined) {
				t.Errorf("build output build/%s does not append {{.HOST_EXE}}", spec)
			}
			// Strip the with-suffix occurrences, then any bare `-o build/<name> `
			// that remains is a suffix-less regression. (The linux sibling
			// `-o build/aileron-mcp-linux-...` does not match `-o build/aileron-mcp `
			// because of the trailing space, so it is not a false positive.)
			stripped := hostArchOutputRe(spec).ReplaceAllString(joined, "")
			if bareHostArchOutputRe(spec).MatchString(stripped) {
				t.Errorf("a suffix-less `-o build/%s ` remains (Windows regression)", spec)
			}
		})
	}
}

// TestTaskfileLinuxSiblingExtensionless pins E1c: the forced GOOS=linux sandbox
// sibling stays extensionless (it is bind-mounted into a Linux container, where
// `.exe` would be wrong).
func TestTaskfileLinuxSiblingExtensionless(t *testing.T) {
	root := loadTaskfile(t)
	cmds := mapGet(taskNode(t, root, "build:mcp"), "cmds")
	joined := strings.Join(allScalars(cmds), "\n")
	re := regexp.MustCompile(`-o build/aileron-mcp-linux-\{\{\s*\.HOST_GOARCH\s*\}\} `)
	if !re.MatchString(joined) {
		t.Error("build:mcp linux sandbox sibling must stay extensionless (-o build/aileron-mcp-linux-{{.HOST_GOARCH}})")
	}
	// And it must NOT carry {{.HOST_EXE}}.
	if regexp.MustCompile(`-o build/aileron-mcp-linux-\{\{\s*\.HOST_GOARCH\s*\}\}\{\{\s*\.HOST_EXE\s*\}\}`).MatchString(joined) {
		t.Error("linux sandbox sibling must not append {{.HOST_EXE}}")
	}
}

// TestTaskfileMcpSetupRegistersHostExe pins E1d (registration half): mcp:setup
// registers ./build/aileron-mcp{{.HOST_EXE}} so it matches the built file on
// Windows.
func TestTaskfileMcpSetupRegistersHostExe(t *testing.T) {
	root := loadTaskfile(t)
	cmds := mapGet(taskNode(t, root, "mcp:setup"), "cmds")
	joined := strings.Join(allScalars(cmds), "\n")
	re := regexp.MustCompile(`\./build/aileron-mcp\{\{\s*\.HOST_EXE\s*\}\}`)
	if !re.MatchString(joined) {
		t.Error("mcp:setup must register ./build/aileron-mcp{{.HOST_EXE}}")
	}
}

// TestTaskfileLaunchAileronBinHostExe pins E1d (launch half) + E1g: both launch
// scenarios point AILERON_BIN at '{{.ROOT_DIR}}/build/aileron{{.HOST_EXE}}',
// asserted on the parsed task-level env value (exactly the two launch targets).
func TestTaskfileLaunchAileronBinHostExe(t *testing.T) {
	root := loadTaskfile(t)
	targets := []string{"test:system:launch:codex", "test:system:launch:claude"}
	want := "{{.ROOT_DIR}}/build/aileron{{.HOST_EXE}}"
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			env := mapGet(taskNode(t, root, target), "env")
			got := scalarValue(mapGet(env, "AILERON_BIN"))
			if got != want {
				t.Errorf("%s: AILERON_BIN = %q, want %q", target, got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (F) launch leaves are shell-free Go (#1626)
// ---------------------------------------------------------------------------

// scenarioInvocationRe matches the shell-free leaf cmd that drives the Go
// scenario driver: `go run ./test/system/scenario <agent>`. The agent token is
// captured so the per-target assertion can confirm the right one.
var scenarioInvocationRe = regexp.MustCompile(`go run \./test/system/scenario\s+(codex|claude)\b`)

// sourcedShellScriptRe matches a retired sourced-bash leaf: a `sh ` (or
// `sh -x` etc.) invocation of a `.sh` file, the exact shape #1626 retires
// (`sh "{{.ROOT_DIR}}/test/system/codex.sh"`). Anchored at the start of the
// scalar (after optional leading whitespace) so a `.sh` mention inside prose
// does not false-positive; the broader `.sh` ban below catches the rest.
var sourcedShellScriptRe = regexp.MustCompile(`^\s*sh\b.*\.sh\b`)

// dotShReferenceRe matches any `.sh` filename token, used to assert no retired
// bash file survives anywhere under a launch task's cmd subtree.
var dotShReferenceRe = regexp.MustCompile(`\.sh\b`)

// TestTaskfileLaunchCmdShellFree pins the #1626 contract: each launch leaf runs
// the shell-free Go scenario driver, and no retired `sh "...sh"` invocation (or
// any `.sh` reference) survives under either launch task's cmds. This guards the
// shell-free posture going forward: reverting a leaf to `cmd: sh ".../codex.sh"`
// fails (b), and dropping the Go driver invocation fails (a).
func TestTaskfileLaunchCmdShellFree(t *testing.T) {
	root := loadTaskfile(t)
	targets := map[string]string{
		"test:system:launch:codex":  "codex",
		"test:system:launch:claude": "claude",
	}

	for target, wantAgent := range targets {
		t.Run(target, func(t *testing.T) {
			task := taskNode(t, root, target)
			cmds := mapGet(task, "cmds")
			if cmds == nil || cmds.Kind != yaml.SequenceNode {
				t.Fatalf("%s: no cmds: sequence", target)
			}
			cmdScalars := allScalars(cmds)

			// (a) positive: the terminal scenario-invocation cmd drives
			// `go run ./test/system/scenario <agent>` with the matching agent.
			foundAgent := ""
			for _, s := range cmdScalars {
				if m := scenarioInvocationRe.FindStringSubmatch(s); m != nil {
					foundAgent = m[1]
					break
				}
			}
			if foundAgent == "" {
				t.Errorf("%s: no leaf cmd invokes `go run ./test/system/scenario <agent>`; cmds=%q", target, cmdScalars)
			} else if foundAgent != wantAgent {
				t.Errorf("%s: scenario driver invoked with agent %q, want %q", target, foundAgent, wantAgent)
			}

			// (b) negative: no retired sourced-bash leaf and no `.sh` reference
			// survives anywhere under the task's cmds subtree.
			for _, s := range cmdScalars {
				if sourcedShellScriptRe.MatchString(s) {
					t.Errorf("%s: a sourced-bash leaf survives (retired by #1626): %q", target, s)
				}
				if dotShReferenceRe.MatchString(s) {
					t.Errorf("%s: a `.sh` reference survives under cmds (retired by #1626): %q", target, s)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (G) _test:system:precond is free of Windows-fatal Unix externals (#1653)
// ---------------------------------------------------------------------------

// stripShellComments drops full-line `#` comments and blank lines from a shell
// block scalar, returning only the executable lines. The precond block's own
// prose mentions `tr` and `grep -qx` to explain why they were removed, so a
// flat scan of the raw scalar would false-positive on the documentation; the
// contract is about what the shell actually EXECUTES, so only code lines count.
// A `#` that is not the first non-space token (e.g. inside a string) is left
// intact — none of the precond's external-command shapes embed a literal `#`,
// so this conservative line-level strip is sufficient and never hides code.
func stripShellComments(block string) string {
	var keep []string
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// trExternalRe matches an invocation of the `tr` coreutil: `tr` as a command
// word, i.e. at a statement boundary: the start of any line (the `(?m)`
// flag makes `^` match each line, not just the block start), or after `|`,
// `;`, `&`, `(`, or `$(`. Anchoring on the boundary avoids matching `tr`
// inside an identifier
// (e.g. `authfile`, `entries`, `vaultentry`) while catching the real
// `printf ... | tr '\\' '/'` shape #1653 removed.
var trExternalRe = regexp.MustCompile(`(?m)(?:^|[|;&(])\s*tr\s`)

// grepExternalRe matches an invocation of the `grep` coreutil at a statement or
// pipeline boundary, catching `... | grep -qx ...` (the retired vault-entry
// membership shape) without matching the word inside a comment or string.
var grepExternalRe = regexp.MustCompile(`(?m)(?:^|[|;&(])\s*grep\s`)

// TestTaskfilePrecondNoUnixExternals pins the #1653 contract: the
// `_test:system:precond` block shells out to NO Unix-only external that a stock
// Windows host may lack — specifically not `tr` (which failed the whole run
// with exit 127) and not `grep` (the next such trap, called out in the issue).
// The check is over the EXECUTED lines of the block (comments stripped), so the
// block may still document why those externals were removed. The normalization
// and membership-test behavior these externals previously provided is covered
// behaviorally by TestTaskBehaviorPrecondShellBuiltins below.
func TestTaskfilePrecondNoUnixExternals(t *testing.T) {
	root := loadTaskfile(t)
	task := taskNode(t, root, "_test:system:precond")
	cmds := mapGet(task, "cmds")
	if cmds == nil || cmds.Kind != yaml.SequenceNode {
		t.Fatal("_test:system:precond: no cmds: sequence")
	}
	code := stripShellComments(strings.Join(allScalars(cmds), "\n"))

	if loc := trExternalRe.FindString(code); loc != "" {
		t.Errorf("_test:system:precond executes `tr` (Unix external, not on a stock Windows PATH — #1653); "+
			"use go-task's shell builtin `${var//\\\\//}` instead. Matched: %q", strings.TrimSpace(loc))
	}
	if loc := grepExternalRe.FindString(code); loc != "" {
		t.Errorf("_test:system:precond executes `grep` (Unix external, not on a stock Windows PATH — #1653); "+
			"use a shell `while read`+`case` membership test instead. Matched: %q", strings.TrimSpace(loc))
	}
}

// TestPrecondExternalRegexSensitivity pins the matcher used by
// TestTaskfilePrecondNoUnixExternals: it must flag a `tr`/`grep` invocation at
// ANY statement boundary, including the start of a later line in the multiline
// block (the `(?m)` anchoring), not only after a pipe. A line-leading external
// is a plausible future regression shape (e.g. a bare `tr ... < file` on its
// own line), so the guard would be hollow if it only caught the piped form.
// Conversely it must NOT flag the substring `tr`/`grep` inside an identifier.
func TestPrecondExternalRegexSensitivity(t *testing.T) {
	cases := []struct {
		name  string
		code  string
		re    *regexp.Regexp
		match bool
	}{
		{"tr piped", "printf '%s' \"$x\" | tr '\\\\' '/'", trExternalRe, true},
		{"tr line-leading on later line", "authfile=x\ntr 'a' 'b' < f", trExternalRe, true},
		{"tr after semicolon", "x=1; tr a b", trExternalRe, true},
		{"tr inside identifier authfile", "authfile=\"$HOME/x\"", trExternalRe, false},
		{"tr inside identifier entries", "entries=\"$(cmd)\"", trExternalRe, false},
		{"grep piped on later line", "aileron vault list >/dev/null\naileron vault list | grep -qx \"$e\"", grepExternalRe, true},
		{"grep line-leading", "grep -q foo bar", grepExternalRe, true},
		{"grep absent", "while IFS= read -r line; do :; done", grepExternalRe, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.re.MatchString(c.code)
			if got != c.match {
				t.Errorf("re.MatchString(%q) = %v, want %v", c.code, got, c.match)
			}
		})
	}
}

// TestTaskBehaviorPrecondShellBuiltins pins the behavioral half of #1653: the
// two builtins that replaced `tr` and `grep -qx` produce the correct results
// under go-task's embedded shell, with a PATH that omits `tr` and `grep`
// (simulating the stock Windows host where the bug manifested). This is the
// regression proof: before the fix the precond shelled out to those externals
// and would exit 127 under this PATH; after, the builtins resolve natively.
func TestTaskBehaviorPrecondShellBuiltins(t *testing.T) {
	sandbox := sandboxPATH(t)
	taskEnvPATH := append(filterEnv(os.Environ(), "PATH"), "PATH="+sandbox)

	// Sim-validity: the sandbox PATH must truly lack tr and grep, else the test
	// proves nothing about the no-external posture.
	t.Run("sandbox PATH excludes tr and grep", func(t *testing.T) {
		if resolvableUnder(sandbox, "tr") {
			t.Error("sandbox PATH unexpectedly resolves `tr`")
		}
		if resolvableUnder(sandbox, "grep") {
			t.Error("sandbox PATH unexpectedly resolves `grep`")
		}
	})

	// (1) Backslash normalization: ${authfile//\\//} turns the mixed
	// C:\Users\alr/.codex/auth.json git-bash form into a single-separator path,
	// exactly the substitution the retired `tr '\\' '/'` performed.
	t.Run("backslash normalization without tr", func(t *testing.T) {
		tf := `version: '3'
tasks:
  r:
    silent: true
    cmds:
      - |
        authfile='C:\Users\alr/.codex/auth.json'
        authfile="${authfile//\\//}"
        echo "RESOLVED=[$authfile]"
`
		out := runTask(t, tf, "r", taskEnvPATH)
		got := extractBracketed(out, "RESOLVED=[", "]")
		if want := "C:/Users/alr/.codex/auth.json"; got != want {
			t.Errorf("normalized path = %q, want %q; output:\n%s", got, want, out)
		}
	})

	// (2) Whole-line membership test: the while-read+case loop reports a present
	// entry as found and a substring-only near-match as NOT found, preserving the
	// exact-line semantics of the retired `grep -qx`.
	t.Run("whole-line membership without grep", func(t *testing.T) {
		tf := `version: '3'
tasks:
  r:
    silent: true
    vars:
      TARGET: '{{.TARGET}}'
    cmds:
      - |
        entries="agents/codex/oauth
        agents/claude/oauth
        other/thing"
        found=
        while IFS= read -r line; do
          case "$line" in
            "{{.TARGET}}") found=1; break ;;
          esac
        done <<< "$entries"
        echo "RESOLVED=[${found:-none}]"
`
		cases := []struct {
			target string
			want   string
		}{
			{"agents/claude/oauth", "1"},     // exact present entry
			{"agents/missing/oauth", "none"}, // absent entry
			{"agents/claude", "none"},        // substring of a line, not a whole line
		}
		for _, c := range cases {
			t.Run(c.target, func(t *testing.T) {
				env := append(filterEnv(os.Environ(), "PATH"), "PATH="+sandbox)
				bin := taskBin(t)
				dir := t.TempDir()
				path := filepath.Join(dir, "Taskfile.yml")
				if err := os.WriteFile(path, []byte(tf), 0o600); err != nil {
					t.Fatalf("writing temp Taskfile: %v", err)
				}
				cmd := exec.Command(bin, "-t", path, "r", "TARGET="+c.target)
				cmd.Env = env
				outB, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("`task r TARGET=%s` failed: %v\noutput:\n%s", c.target, err, outB)
				}
				got := extractBracketed(string(outB), "RESOLVED=[", "]")
				if got != c.want {
					t.Errorf("membership for %q = %q, want %q; output:\n%s", c.target, got, c.want, outB)
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// (A) behavioral: go-task env scope semantics
// ---------------------------------------------------------------------------

// runTask writes taskfileContents to a temp file, runs `task -t <file> <name>`
// with the given env (nil = inherit the test process env), and returns the
// combined stdout+stderr. It fails loud if `task` is absent or the run errors.
func runTask(t *testing.T, taskfileContents, taskName string, env []string) string {
	t.Helper()
	bin := taskBin(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "Taskfile.yml")
	if err := os.WriteFile(path, []byte(taskfileContents), 0o600); err != nil {
		t.Fatalf("writing temp Taskfile: %v", err)
	}
	cmd := exec.Command(bin, "-t", path, taskName)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`task %s` failed: %v\noutput:\n%s", taskName, err, out)
	}
	return string(out)
}

// TestTaskBehaviorEnvScope pins half A of #1485 via the real `task` binary: a
// task-scoped env: reaches the cmd's process environment, and a command-scoped
// env: is silently dropped. The probe is an `echo $AILERON_BIN` cmd whose `$`
// expansion is performed by go-task's built-in portable shell (mvdan.cc/sh), so
// it runs on Windows without an external `sh` (no `sh probe.sh`). This reads the
// command's actual environment — exactly the env scope the shell test's probe.sh
// read via `$AILERON_BIN` — NOT a go-task template var (task-scoped env: is not
// exposed as a `.AILERON_BIN` template var, only as a process env var). A
// task-scoped `env:` populates that environment; a command-scoped `env:` is
// ignored by go-task, so the expansion is empty.
func TestTaskBehaviorEnvScope(t *testing.T) {
	t.Run("task-scoped env reaches the command", func(t *testing.T) {
		tf := `version: '3'
tasks:
  t:
    env:
      AILERON_BIN: 'task-scope-value'
    cmds:
      - 'echo PROBE_BIN=[$AILERON_BIN]'
`
		// Run with a controlled env that lacks AILERON_BIN, so the only source of
		// the value is the task-scoped env: under test.
		env := filterEnv(os.Environ(), "AILERON_BIN")
		out := runTask(t, tf, "t", env)
		if !strings.Contains(out, "PROBE_BIN=[task-scope-value]") {
			t.Errorf("task-scoped env did not reach the command; output:\n%s", out)
		}
	})

	t.Run("command-scoped env is dropped", func(t *testing.T) {
		// go-task silently ignores a cmds[].env:, so the command's environment
		// never carries AILERON_BIN and `$AILERON_BIN` expands to empty. This
		// documents the footgun the task-scope fix routes around.
		tf := `version: '3'
tasks:
  t:
    cmds:
      - cmd: 'echo PROBE_BIN=[$AILERON_BIN]'
        env:
          AILERON_BIN: 'cmd-scope-value'
`
		// Run with a controlled env that does NOT carry AILERON_BIN, so the only
		// way the value could surface is via the (dropped) command-scoped env:.
		env := filterEnv(os.Environ(), "AILERON_BIN")
		out := runTask(t, tf, "t", env)
		if strings.Contains(out, "PROBE_BIN=[cmd-scope-value]") {
			t.Errorf("command-scoped env unexpectedly reached the command; output:\n%s", out)
		}
		if !strings.Contains(out, "PROBE_BIN=[]") {
			t.Errorf("expected command-scoped env to resolve empty (PROBE_BIN=[]); output:\n%s", out)
		}
	})
}

// filterEnv returns env with any entry whose key equals `drop` removed.
func filterEnv(env []string, drop string) []string {
	out := make([]string, 0, len(env))
	prefix := drop + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ---------------------------------------------------------------------------
// (D) behavioral: vars resolve with date+mktemp off PATH (#1547)
// ---------------------------------------------------------------------------

// sandboxPATH builds a PATH directory containing ONLY a copy of the `task`
// binary, so a process run under it cannot resolve `date` or `mktemp` (nor any
// external `sh`). This mirrors go-task's Windows embedded-shell condition more
// faithfully than the shell test's symlink sandbox did: the pure-template
// SESSION/WORKSPACE vars resolve entirely inside go-task (its built-in
// mvdan.cc/sh interpreter runs the `echo`, the funcs are template-native), so no
// external shell is required — and requiring one would make this Windows-fatal
// for no benefit. No symlinks (repo-forbidden): `task` is copied. Returns the
// directory.
func sandboxPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src, err := exec.LookPath("task")
	if err != nil {
		t.Fatalf("resolving `task` for PATH sandbox: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading `task` for PATH sandbox: %v", err)
	}
	dst := filepath.Join(dir, "task")
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("writing `task` into PATH sandbox: %v", err)
	}
	return dir
}

// resolvableUnder reports whether `name` would be resolvable as an executable
// when PATH is exactly dir. It does not mutate the process environment (so it is
// safe under parallel tests), inspecting dir's entries directly instead of
// relying on exec.LookPath's reliance on the global PATH.
func resolvableUnder(dir, name string) bool {
	candidate := filepath.Join(dir, name)
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return false
	}
	// On Unix an executable bit is required; on Windows os.Stat existence with an
	// executable extension suffices. The sandbox only ever holds the two binaries
	// we copy with 0o755, so a plain existence check is sufficient for the sim
	// validity assertion (date/mktemp are simply absent from dir).
	return info.Mode()&0o111 != 0 || runtime.GOOS == "windows"
}

// TestTaskBehaviorVarsResolveWithoutDateMktemp pins half D of #1547: the real
// SESSION/WORKSPACE var definitions (extracted from the parsed Taskfile, so the
// behavioral case tracks the structural one) resolve to non-empty values under
// a PATH that omits `date`/`mktemp`, simulating go-task's Windows embedded shell.
func TestTaskBehaviorVarsResolveWithoutDateMktemp(t *testing.T) {
	root := loadTaskfile(t)
	taskEnvPATH := func(dir string) []string {
		return append(filterEnv(os.Environ(), "PATH"), "PATH="+dir)
	}

	sandbox := sandboxPATH(t)

	// Sim-validity check, mirroring the shell: the sandbox PATH must truly lack
	// date and mktemp, else the test proves nothing.
	t.Run("sandbox PATH excludes date and mktemp", func(t *testing.T) {
		if resolvableUnder(sandbox, "date") {
			t.Error("sandbox PATH unexpectedly resolves `date`")
		}
		if resolvableUnder(sandbox, "mktemp") {
			t.Error("sandbox PATH unexpectedly resolves `mktemp`")
		}
	})

	targets := []string{"test:system:smoke", "test:system:launch:codex", "test:system:launch:claude"}
	for _, target := range targets {
		task := taskNode(t, root, target)
		for _, key := range []string{"SESSION", "WORKSPACE"} {
			t.Run(target+"/"+key, func(t *testing.T) {
				// Pull the inline scalar definition (the form the structural check
				// pins). Both SESSION and WORKSPACE are inline templates today; a
				// missing or non-scalar var must FAIL (not silently skip), else the
				// behavioral guard would evaporate if a target were re-shaped.
				val := mapGet(mapGet(task, "vars"), key)
				if val == nil {
					t.Fatalf("%s: var %s is not defined", target, key)
				}
				if val.Kind != yaml.ScalarNode {
					t.Fatalf("%s: var %s must be an inline scalar, got a %v definition", target, key, val.Kind)
				}
				def := val.Value
				// Marshal the generated Taskfile via yaml so the extracted var
				// definition is re-emitted with correct quoting regardless of the
				// template's contents (no hand-rolled single-quote escaping).
				tf := genVarTaskfile(t, key, def)
				out := runTask(t, tf, "r", taskEnvPATH(sandbox))
				resolved := extractBracketed(out, "RESOLVED=[", "]")
				if resolved == "" {
					t.Errorf("%s %s resolved empty with date/mktemp off PATH; output:\n%s", target, key, out)
				}
			})
		}
	}
}

// genVarTaskfile builds a one-task Taskfile that defines var `key` to `def` and
// echoes its resolved value, using yaml.Marshal so `def` is quoted correctly
// whatever characters it contains. The cmd echo line is injected verbatim (a
// go-task `{{...}}` template, not a YAML value to escape).
func genVarTaskfile(t *testing.T, key, def string) string {
	t.Helper()
	model := map[string]any{
		"version": "3",
		"tasks": map[string]any{
			"r": map[string]any{
				"vars": map[string]any{key: def},
				"cmds": []any{"echo RESOLVED=[{{." + key + "}}]"},
			},
		},
	}
	out, err := yaml.Marshal(model)
	if err != nil {
		t.Fatalf("marshaling generated Taskfile: %v", err)
	}
	return string(out)
}

// extractBracketed returns the substring of s between the first occurrence of
// open and the next occurrence of close after it, or "".
func extractBracketed(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// ---------------------------------------------------------------------------
// (E2) behavioral: HOST_EXE resolves under a forced GOOS (#1590)
// ---------------------------------------------------------------------------

// resolveHostExePath drives a tiny Taskfile whose HOST_EXE var is `sh: go env
// GOEXE`, under a forced GOOS, and returns the resolved `build/aileron<suffix>`
// path. Forcing GOOS makes `go env GOEXE` deterministic, so the case is
// hermetic regardless of host OS.
func resolveHostExePath(t *testing.T, goos string) string {
	t.Helper()
	goBin(t) // fail loud if `go` is absent
	tf := `version: '3'
vars:
  HOST_EXE:
    sh: go env GOEXE
tasks:
  r:
    cmds:
      - 'echo RESOLVED=[build/aileron{{.HOST_EXE}}]'
`
	env := append(filterEnv(os.Environ(), "GOOS"), "GOOS="+goos)
	out := runTask(t, tf, "r", env)
	return extractBracketed(out, "RESOLVED=[", "]")
}

// TestTaskBehaviorHostExeResolution pins half E2 of #1590: GOOS=windows yields a
// `.exe` suffix, and the native GOOS yields the host's own GOEXE.
func TestTaskBehaviorHostExeResolution(t *testing.T) {
	t.Run("GOOS=windows resolves to .exe", func(t *testing.T) {
		got := resolveHostExePath(t, "windows")
		if got != "build/aileron.exe" {
			t.Errorf("GOOS=windows path = %q, want build/aileron.exe", got)
		}
	})

	t.Run("native GOOS resolves to host GOEXE", func(t *testing.T) {
		// Determine the native GOEXE via `go env` directly for the expectation.
		bin := goBin(t)
		goosOut, err := exec.Command(bin, "env", "GOOS").Output()
		if err != nil {
			t.Fatalf("go env GOOS: %v", err)
		}
		nativeGOOS := strings.TrimSpace(string(goosOut))
		goexeOut, err := exec.Command(bin, "env", "GOEXE").Output()
		if err != nil {
			t.Fatalf("go env GOEXE: %v", err)
		}
		nativeGOEXE := strings.TrimSpace(string(goexeOut))

		got := resolveHostExePath(t, nativeGOOS)
		want := "build/aileron" + nativeGOEXE
		if got != want {
			t.Errorf("native path = %q, want %q", got, want)
		}
	})
}
