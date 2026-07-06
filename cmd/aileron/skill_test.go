package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/resolver"
)

// repoRootForTest walks up to the directory containing go.work so tests can
// read the committed worked example as a known-valid credentialed skill.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.work from %s", filepath.Dir(file))
		}
		dir = parent
	}
}

const instructionOnlySkillMd = `---
name: rubber-duck
description: Instruction-only skill.
---

# Rubber Duck
Explain it out loud.
`

// withTempStore points the CLI at a temp skill store for the test.
func withTempStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := skillStoreDir
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = orig })
	return dir
}

// writeSkillFile materializes a SKILL.md in a temp dir and returns its
// directory path (a valid local install source).
func writeSkillFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// stubFetcher swaps newSkillFetcher with one returning the given actions
// (or error), recording whether it was constructed.
func stubFetcher(t *testing.T, actions []resolver.Action, fetchErr, ctorErr error) *bool {
	t.Helper()
	constructed := false
	orig := newSkillFetcher
	newSkillFetcher = func() (resolver.Fetcher, error) {
		constructed = true
		if ctorErr != nil {
			return nil, ctorErr
		}
		return resolver.FetcherFunc(func(ctx context.Context) ([]resolver.Action, error) {
			return actions, fetchErr
		}), nil
	}
	t.Cleanup(func() { newSkillFetcher = orig })
	return &constructed
}

func exampleSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForTest(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	return writeSkillFile(t, string(raw))
}

func TestRunSkillInstall_InstructionOnly_Clean(t *testing.T) {
	store := withTempStore(t)
	src := writeSkillFile(t, instructionOnlySkillMd)
	// A fetcher constructor that fails if touched proves the daemon client
	// is never even constructed for an instruction-only install.
	constructed := stubFetcher(t, nil, nil, errors.New("must not be reached"))

	var stdout, stderr bytes.Buffer
	if code := runSkillInstall([]string{src}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if *constructed {
		t.Error("daemon fetcher must not be constructed for instruction-only install")
	}
	if stderr.Len() != 0 {
		t.Errorf("instruction-only install must be silent on stderr, got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed skill \"rubber-duck\"") {
		t.Errorf("stdout = %q", stdout.String())
	}
	// A local install lands a pre-freeze SKILL.md that cannot be bound until it
	// is frozen, so the operator must be pointed at freeze-then-bind rather than
	// bind directly (the OCI path's pointer).
	if !strings.Contains(stdout.String(), "aileron skill freeze \"rubber-duck\"") ||
		!strings.Contains(stdout.String(), "aileron skill bind \"rubber-duck\"") {
		t.Errorf("local install must print a freeze-then-bind hint, stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(store, "rubber-duck", "SKILL.md")); err != nil {
		t.Errorf("skill not written: %v", err)
	}
}

func TestRunSkillInstall_CredentialedSatisfiable_NoWarning(t *testing.T) {
	withTempStore(t)
	src := exampleSource(t)
	stubFetcher(t, []resolver.Action{
		{Name: "query-series", Requires: resolver.Requires{Connectors: []resolver.Connector{{Name: "github://aileron/metrics"}}}},
		{Name: "create-issue", Requires: resolver.Requires{Connectors: []resolver.Connector{{Name: "github://aileron/tracker"}}}},
	}, nil, nil)

	var stdout, stderr bytes.Buffer
	if code := runSkillInstall([]string{src}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("satisfiable install must not warn, stderr=%s", stderr.String())
	}
}

func TestRunSkillInstall_Unsatisfiable_WarnsButExits0(t *testing.T) {
	withTempStore(t)
	src := exampleSource(t)
	// No actions installed: both refs are unsatisfiable.
	stubFetcher(t, nil, nil, nil)

	var stdout, stderr bytes.Buffer
	code := runSkillInstall([]string{src}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unsatisfiable install must still exit 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not satisfiable") {
		t.Errorf("expected a degrade warning on stderr, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "aileron:metrics.query_series") {
		t.Errorf("warning should name the unsatisfied ref, got: %s", stderr.String())
	}
}

func TestRunSkillInstall_DaemonDownInstructionOnly_Clean(t *testing.T) {
	withTempStore(t)
	src := writeSkillFile(t, instructionOnlySkillMd)
	// Daemon-down via a constructor error — must never be reached.
	stubFetcher(t, nil, nil, errors.New("daemon down"))

	var stdout, stderr bytes.Buffer
	if code := runSkillInstall([]string{src}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stderr.String(), "daemon") {
		t.Errorf("instruction-only install must not surface daemon connectivity, got: %s", stderr.String())
	}
}

func TestRunSkillInstall_DaemonDownCredentialed_DegradesNotFails(t *testing.T) {
	withTempStore(t)
	src := exampleSource(t)
	// Daemon unreachable when resolution IS needed: degrade (warn), exit 0.
	stubFetcher(t, nil, nil, errors.New("connection refused"))

	var stdout, stderr bytes.Buffer
	code := runSkillInstall([]string{src}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("daemon-down credentialed install must degrade not fail, got %d", code)
	}
	if !strings.Contains(stderr.String(), "could not reach the daemon") {
		t.Errorf("expected a daemon-unreachable warning, got: %s", stderr.String())
	}
}

func TestRunSkillList(t *testing.T) {
	store := withTempStore(t)
	// Pre-populate two skills.
	for _, name := range []string{"beta", "alpha"} {
		dir := filepath.Join(store, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\n---\nx\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runSkillList(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// Sorted output.
	if got := stdout.String(); got != "alpha\nbeta\n" {
		t.Errorf("list output = %q", got)
	}
}

func TestRunSkillList_Empty(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	if code := runSkillList(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "No skills installed") {
		t.Errorf("empty list = %q", stdout.String())
	}
}

func TestRunSkill_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runSkill([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("unknown subcommand exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown skill command") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunSkillInstall_RequiresExactlyOneSource(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	// No source argument.
	if code := runSkillInstall(nil, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("no source exit = %d, want 1", code)
	}
	// Two sources.
	if code := runSkillInstall([]string{"a", "b"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("two sources exit = %d, want 1", code)
	}
}

func TestRunSkillInstall_BadFlag(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	if code := runSkillInstall([]string{"--nope"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("bad flag exit = %d, want 1", code)
	}
}

func TestRunSkillInstall_BadSourceError(t *testing.T) {
	withTempStore(t)
	stubFetcher(t, nil, nil, nil)
	var stdout, stderr bytes.Buffer
	// A nonexistent local path that is not a git URL classifies as a slug,
	// which is not wired, so install surfaces an error and exits 1.
	if code := runSkillInstall([]string{"definitely/not/installed"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("unresolvable source exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("expected an error on stderr, got: %s", stderr.String())
	}
}

func TestNewSkillFetcher_DaemonUnreachableDegrades(t *testing.T) {
	// Exercise the real newSkillFetcher (not the stub) so the lazyFetcher
	// ctor + daemon-unreachable degrade path is covered. Point the daemon
	// base URL at a dead address; resolution must warn, not fail.
	withTempStore(t)
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:1/v1")
	src := exampleSource(t)

	var stdout, stderr bytes.Buffer
	code := runSkillInstall([]string{src}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("daemon-unreachable credentialed install must degrade not fail, got %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not") {
		t.Errorf("expected a daemon-unreachable warning, got: %s", stderr.String())
	}
}

func TestRunSkill_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runSkill(nil, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("no-args exit = %d, want 1", code)
	}
}

func TestRun_DispatchesSkill(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	// `aileron skill list` on an empty store routes through run() dispatch
	// and exits 0.
	if code := run([]string{"skill", "list"}, newTestRegistry(), &stdout, &stderr); code != 0 {
		t.Fatalf("run skill list exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No skills installed") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRun_HelpMentionsSkill(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, newTestRegistry(), &stdout, &stderr); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "aileron skill install") {
		t.Errorf("help must mention skill install, got: %s", stdout.String())
	}
}
