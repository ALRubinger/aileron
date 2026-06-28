package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/version"
)

// sliceFlag is a repeatable string flag used to assert that
// parseInterspersedFlags accumulates a flag.Var across the positional
// boundary (the `--arg`/`--input` shape the real subcommands use).
type sliceFlag []string

func (s *sliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *sliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// TestParseInterspersedFlags_BothOrders is the core regression: flags must
// parse whether they appear before OR after the positional. Go's stdlib
// flag.Parse stops at the first non-flag token, so the flags-after ordering
// regressed before this helper existed.
func TestParseInterspersedFlags_BothOrders(t *testing.T) {
	parse := func(args []string) (string, bool, []string) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(&bytes.Buffer{})
		name := fs.String("name", "", "")
		on := fs.Bool("on", false, "")
		pos, err := parseInterspersedFlags(fs, args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		return *name, *on, pos
	}

	for _, tc := range []struct {
		label string
		args  []string
	}{
		{"flags-first", []string{"--name", "x", "--on", "target"}},
		{"flags-after", []string{"target", "--name", "x", "--on"}},
		{"interspersed", []string{"--name", "x", "target", "--on"}},
		{"equals-after", []string{"target", "--name=x", "--on"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			name, on, pos := parse(tc.args)
			if name != "x" || !on {
				t.Errorf("flags = name:%q on:%v, want x/true", name, on)
			}
			if len(pos) != 1 || pos[0] != "target" {
				t.Errorf("positionals = %v, want [target]", pos)
			}
		})
	}
}

// TestParseInterspersedFlags_RepeatableAccumulatesAcrossPositional locks the
// behavior the issue calls out: a repeatable flag (flag.Var) must accumulate
// values that appear on both sides of the positional, not reset per pass.
func TestParseInterspersedFlags_RepeatableAccumulatesAcrossPositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	var args sliceFlag
	fs.Var(&args, "arg", "repeatable")
	pos, err := parseInterspersedFlags(fs, []string{"--arg", "a=1", "name", "--arg", "b=2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 || pos[0] != "name" {
		t.Fatalf("positionals = %v, want [name]", pos)
	}
	if len(args) != 2 || args[0] != "a=1" || args[1] != "b=2" {
		t.Errorf("repeatable flag = %v, want [a=1 b=2] accumulated across the positional", args)
	}
}

// TestParseInterspersedFlags_ScalarLastWins documents that a scalar flag set
// in a later pass keeps its last value, and one absent from a pass retains
// its prior value.
func TestParseInterspersedFlags_ScalarLastWins(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	v := fs.String("v", "default", "")
	pos, err := parseInterspersedFlags(fs, []string{"name", "--v", "set"})
	if err != nil {
		t.Fatal(err)
	}
	if *v != "set" {
		t.Errorf("scalar = %q, want set", *v)
	}
	if len(pos) != 1 || pos[0] != "name" {
		t.Errorf("positionals = %v", pos)
	}
}

func TestParseInterspersedFlags_NoPositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	on := fs.Bool("on", false, "")
	pos, err := parseInterspersedFlags(fs, []string{"--on"})
	if err != nil {
		t.Fatal(err)
	}
	if !*on {
		t.Error("flag not parsed")
	}
	if len(pos) != 0 {
		t.Errorf("positionals = %v, want none", pos)
	}
}

func TestParseInterspersedFlags_MultiplePositionals(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	on := fs.Bool("on", false, "")
	pos, err := parseInterspersedFlags(fs, []string{"a", "--on", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !*on {
		t.Error("interspersed flag not parsed")
	}
	if len(pos) != 2 || pos[0] != "a" || pos[1] != "b" {
		t.Errorf("positionals = %v, want [a b] in order", pos)
	}
}

func TestParseInterspersedFlags_UnknownFlagErrors(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	if _, err := parseInterspersedFlags(fs, []string{"name", "--nope"}); err == nil {
		t.Error("an unknown flag after the positional must surface a parse error")
	}
}

// TestParseInterspersedFlags_DoubleDashTerminator: tokens after `--` are
// treated as positionals, even when they look like flags.
func TestParseInterspersedFlags_DoubleDashTerminator(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	on := fs.Bool("on", false, "")
	pos, err := parseInterspersedFlags(fs, []string{"--on", "--", "--looks-like-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if !*on {
		t.Error("flag before -- not parsed")
	}
	if len(pos) != 1 || pos[0] != "--looks-like-flag" {
		t.Errorf("positionals = %v, want [--looks-like-flag] (after --)", pos)
	}
}

// --- Subcommand-level regressions: flags placed AFTER the positional ---

// TestRunActionRun_FlagsAfterName is the confirmed bug from #1685: a flag
// after the action name was dropped because flag.Parse stopped at the name.
// Both orderings must now reach the daemon with identical args.
func TestRunActionRun_FlagsAfterName(t *testing.T) {
	for _, tc := range []struct {
		label string
		args  []string
	}{
		{"flags-before", []string{"--arg", "team=ENG", "--json", "linear-issues-create"}},
		{"flags-after", []string{"linear-issues-create", "--arg", "team=ENG", "--json"}},
		{"interspersed", []string{"--arg", "team=ENG", "linear-issues-create", "--json"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "a", Result: ptrString(`{}`)})
			})
			setBindingBase(t, base)

			var stdout, stderr bytes.Buffer
			if code := runActionRun(tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
			}
			if gotPath != "/v1/actions/linear-issues-create/run" {
				t.Errorf("path = %q (name not parsed from %v)", gotPath, tc.args)
			}
			var parsed actionRunRequest
			if err := json.Unmarshal(gotBody, &parsed); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if parsed.Args["team"] != "ENG" {
				t.Errorf("args = %#v, want team=ENG parsed from %v", parsed.Args, tc.args)
			}
		})
	}
}

// TestRunActionRun_RepeatableArgAfterName: the repeatable --arg must
// accumulate values on both sides of the name.
func TestRunActionRun_RepeatableArgAfterName(t *testing.T) {
	var gotBody []byte
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "a", Result: ptrString(`{}`)})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionRun([]string{"--arg", "team=ENG", "linear-issues-create", "--arg", "title=Smoke"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	var parsed actionRunRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if parsed.Args["team"] != "ENG" || parsed.Args["title"] != "Smoke" {
		t.Errorf("args = %#v, want both --arg values accumulated across the name", parsed.Args)
	}
}

// TestRunSkillFreeze_FlagsAfterName is the other confirmed #1685 case:
// `skill freeze <name> --version v` dropped the flag. Both orders must work.
func TestRunSkillFreeze_FlagsAfterName(t *testing.T) {
	for _, tc := range []struct {
		label string
		args  func(key string) []string
	}{
		{"flags-before", func(key string) []string {
			return []string{"--signing-key", key, "--version", "9.9.9", "weekly-metrics-digest"}
		}},
		{"flags-after", func(key string) []string {
			return []string{"weekly-metrics-digest", "--signing-key", key, "--version", "9.9.9"}
		}},
		{"interspersed", func(key string) []string {
			return []string{"--signing-key", key, "weekly-metrics-digest", "--version", "9.9.9"}
		}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			storeDir := withTempStore(t)
			installExample(t, storeDir)
			stubFreezeResolvers(t, fakeFreezeDigest)
			key := writeSigningKey(t)

			var stdout, stderr bytes.Buffer
			if code := runSkillFreeze(tc.args(key), &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Froze skill \"weekly-metrics-digest\"") {
				t.Errorf("name not parsed from args: stdout=%q", stdout.String())
			}
			if !strings.Contains(stdout.String(), "Version:     9.9.9") {
				t.Errorf("--version not parsed regardless of order: stdout=%q", stdout.String())
			}
		})
	}
}

// TestRunSkillLaunch_FlagAfterName: `skill launch <name> --out-dir d` must
// honor the flag placed after the name.
func TestRunSkillLaunch_FlagAfterName(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)
	disp := &fakeLaunchDispatcher{results: map[string]map[string]any{
		"aileron:metrics.query_series": {
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8", "content": "{}",
		},
	}}
	stubLaunchSeams(t, disp)
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	// Flag AFTER the name (the regressing order).
	code := runSkillLaunch([]string{"weekly-metrics-digest", "--out-dir", outDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Launched \"weekly-metrics-digest\"") {
		t.Errorf("name not parsed with flag after it: stdout=%q", stdout.String())
	}
	// --out-dir after the name must still take effect.
	if _, err := os.Stat(filepath.Join(outDir, "filed_issue.json")); err != nil {
		t.Errorf("--out-dir after the name was ignored: %v", err)
	}
}

// TestRunSandboxCheck_FlagAfterCommand: `sandbox check <command> --build=...`
// must accept the flag after the positional command.
func TestRunSandboxCheck_FlagAfterCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	stubSandboxCheckSeams(t, version.Version)
	var out, errb bytes.Buffer
	// Flag AFTER the positional command.
	if code := runSandboxCheck([]string{"claude", "--build=auto"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "support: ok") {
		t.Fatalf("stdout = %q, want support: ok (command + flag both parsed)", out.String())
	}
}

// TestRunHubSearch_FlagBeforeAndAfterQuery: the consolidated helper must keep
// `hub search <query> --type X` working AND accept the flags-first order.
func TestRunHubSearch_FlagBeforeAndAfterQuery(t *testing.T) {
	for _, tc := range []struct {
		label string
		args  []string
	}{
		{"flag-after", []string{"hub", "search", "draft", "--type", "actions"}},
		{"flag-before", []string{"hub", "search", "--type", "actions", "draft"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			hits := 0
			fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
				hits++
				if r.URL.Path != "/hub/actions" {
					t.Errorf("expected only /hub/actions, got %q", r.URL.Path)
				}
				_, _ = io.WriteString(w, twoHubActionsJSON)
			})
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, newTestRegistry(), &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
			}
			if hits != 1 {
				t.Errorf("expected exactly 1 endpoint hit (query+type both parsed), got %d", hits)
			}
		})
	}
}

// TestRunHubList_FlagAfterCategory: the optional positional category and the
// --json flag must parse in either order.
func TestRunHubList_FlagAfterCategory(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, twoHubActionsJSON)
	})
	var stdout, stderr bytes.Buffer
	// category positional THEN --json.
	if code := run([]string{"hub", "list", "actions", "--json"}, newTestRegistry(), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d NDJSON lines, want 2 (--json after category honored):\n%s", len(lines), stdout.String())
	}
}

// TestRunVaultPut_FlagBeforeAndAfterPath: `vault put <path> --from-file f`
// and the flags-first order must both store the bytes.
func TestRunVaultPut_FlagBeforeAndAfterPath(t *testing.T) {
	for _, tc := range []struct {
		label string
		args  func(credFile string) []string
	}{
		{"path-then-flag", func(credFile string) []string {
			return []string{"put", "agents/claude/oauth", "--from-file", credFile}
		}},
		{"flag-then-path", func(credFile string) []string {
			return []string{"put", "--from-file", credFile, "agents/claude/oauth"}
		}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var received []byte
			fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/vault/agents/claude/credentials" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body agentCredentialsBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				received = body.Value
				w.WriteHeader(http.StatusNoContent)
			})
			credFile := filepath.Join(t.TempDir(), "creds.json")
			if err := os.WriteFile(credFile, []byte("tok"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := runVault(tc.args(credFile), strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
			}
			if string(received) != "tok" {
				t.Errorf("server received %q, want tok (path+flag parsed in either order)", received)
			}
		})
	}
}

// TestRunVaultDelete_FlagBeforeAndAfterPath: `vault delete <path> --yes` and
// the flags-first order must both delete without prompting.
func TestRunVaultDelete_FlagBeforeAndAfterPath(t *testing.T) {
	for _, tc := range []struct {
		label string
		args  []string
	}{
		{"path-then-flag", []string{"delete", "agents/codex/oauth", "--yes"}},
		{"flag-then-path", []string{"delete", "--yes", "agents/codex/oauth"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			gotDelete := false
			fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/codex/credentials" {
					gotDelete = true
					w.WriteHeader(http.StatusNoContent)
					return
				}
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			})
			var stdout, stderr bytes.Buffer
			// strings.NewReader("") so any (erroneous) prompt would read EOF, not "y".
			if code := runVault(tc.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
			}
			if !gotDelete {
				t.Errorf("DELETE not issued (path+--yes must parse in either order)")
			}
		})
	}
}
