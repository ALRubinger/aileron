package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/resolver"
)

// repoRoot walks up to the directory containing go.work so tests can read
// the committed worked example as a known-valid credentialed skill.
func repoRoot(t *testing.T) string {
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

// writeSkill writes a SKILL.md into a fresh temp dir and returns the dir.
func writeSkill(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// credentialedSkillDir copies the committed worked example into a temp dir
// so install can resolve its real requires refs.
func credentialedSkillDir(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	return writeSkill(t, string(raw))
}

const instructionOnly = `---
name: rubber-duck
description: Instruction-only skill, no aileron block.
---

# Rubber Duck
Explain the problem out loud.
`

// recordingFetcher records whether FetchActions was called, so tests can
// assert the resolver/daemon seam is NOT consulted for instruction-only
// installs.
type recordingFetcher struct {
	called  bool
	actions []resolver.Action
	err     error
}

func (r *recordingFetcher) FetchActions(ctx context.Context) ([]resolver.Action, error) {
	r.called = true
	return r.actions, r.err
}

func TestInstallInstructionOnlyClean(t *testing.T) {
	s := New(t.TempDir())
	src := writeSkill(t, instructionOnly)

	// A fetcher that fails if touched proves instruction-only installs
	// never reach the daemon (P2 isolation: daemon-down must be silent).
	fetcher := &recordingFetcher{err: errors.New("daemon down")}

	res, err := s.Install(context.Background(), src, InstallOptions{Fetcher: fetcher})
	if err != nil {
		t.Fatalf("instruction-only install must be clean: %v", err)
	}
	if fetcher.called {
		t.Error("resolver/daemon seam was consulted for an instruction-only skill")
	}
	if !res.InstructionOnly {
		t.Error("result should be marked InstructionOnly")
	}
	if res.ResolverConsulted {
		t.Error("ResolverConsulted should be false for instruction-only")
	}
	if res.Degraded() {
		t.Error("instruction-only install must not degrade")
	}
	if res.Name != "rubber-duck" {
		t.Errorf("name = %q", res.Name)
	}
	// Content landed in the store.
	if _, err := os.Stat(filepath.Join(res.Dir, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md not written to store: %v", err)
	}
	names, _ := s.List()
	if len(names) != 1 || names[0] != "rubber-duck" {
		t.Errorf("List after install = %v", names)
	}
}

func TestInstallCredentialedSatisfiable(t *testing.T) {
	s := New(t.TempDir())
	src := credentialedSkillDir(t)

	// Satisfy both refs the worked example declares.
	fetcher := &recordingFetcher{actions: []resolver.Action{
		{Name: "query-series", Requires: resolver.Requires{Connectors: []resolver.Connector{{Name: "github://aileron/metrics"}}}},
		{Name: "create-issue", Requires: resolver.Requires{Connectors: []resolver.Connector{{Name: "github://aileron/tracker"}}}},
	}}

	res, err := s.Install(context.Background(), src, InstallOptions{Fetcher: fetcher})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !fetcher.called || !res.ResolverConsulted {
		t.Error("credentialed install must consult the resolver")
	}
	if res.Degraded() {
		t.Errorf("expected no degrade, unsatisfied = %v", res.Unsatisfied)
	}
}

func TestInstallCredentialedUnsatisfiableStillInstalls(t *testing.T) {
	s := New(t.TempDir())
	src := credentialedSkillDir(t)

	// Empty action list: nothing resolves. Degrade, not block.
	fetcher := &recordingFetcher{}

	res, err := s.Install(context.Background(), src, InstallOptions{Fetcher: fetcher})
	if err != nil {
		t.Fatalf("unsatisfiable install must still succeed: %v", err)
	}
	if !res.Degraded() {
		t.Error("expected degrade with unsatisfiable refs")
	}
	if len(res.Unsatisfied) != 2 {
		t.Errorf("unsatisfied = %v, want 2", res.Unsatisfied)
	}
	// The skill is installed despite the degrade.
	if _, err := os.Stat(filepath.Join(res.Dir, "SKILL.md")); err != nil {
		t.Errorf("skill must be installed despite degrade: %v", err)
	}
}

func TestInstallCredentialedNoFetcherDegrades(t *testing.T) {
	s := New(t.TempDir())
	src := credentialedSkillDir(t)

	// nil Fetcher: install-anyway, all refs recorded unsatisfied, no panic.
	res, err := s.Install(context.Background(), src, InstallOptions{})
	if err != nil {
		t.Fatalf("install with no fetcher: %v", err)
	}
	if res.ResolverConsulted {
		t.Error("no fetcher means the resolver is not consulted")
	}
	if len(res.Unsatisfied) != 2 {
		t.Errorf("unsatisfied = %v, want all refs", res.Unsatisfied)
	}
}

func TestInstallReinstallOverwrites(t *testing.T) {
	s := New(t.TempDir())
	src := writeSkill(t, instructionOnly)

	if _, err := s.Install(context.Background(), src, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	// Re-install of the same name is idempotent (overwrite).
	if _, err := s.Install(context.Background(), src, InstallOptions{}); err != nil {
		t.Fatalf("re-install must overwrite: %v", err)
	}
	names, _ := s.List()
	if len(names) != 1 {
		t.Errorf("re-install should not duplicate, got %v", names)
	}
}

func TestInstallCopiesAccompanyingFiles(t *testing.T) {
	s := New(t.TempDir())
	// A skill that carries a reference file in a subdirectory: the store
	// must copy the whole tree, not just SKILL.md.
	srcDir := writeSkill(t, instructionOnly)
	if err := os.MkdirAll(filepath.Join(srcDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "references", "guide.md"), []byte("# Guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := s.Install(context.Background(), srcDir, InstallOptions{})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "references", "guide.md")); err != nil {
		t.Errorf("accompanying file not copied into store: %v", err)
	}
}

func TestListErrorOnFileRoot(t *testing.T) {
	// A store root that is a regular file (not a directory) surfaces an
	// error from List rather than being silently treated as empty.
	f := filepath.Join(t.TempDir(), "rootfile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f).List(); err == nil {
		t.Error("expected List error when root is a file")
	}
}

func TestInstallSlugNotWired(t *testing.T) {
	s := New(t.TempDir())
	// A bare owner/name with no matching local path classifies as a slug.
	_, err := s.Install(context.Background(), "acme/weekly-digest", InstallOptions{})
	if !errors.Is(err, ErrSlugNotWired) {
		t.Errorf("expected ErrSlugNotWired, got %v", err)
	}
}

func TestInstallSkillWithoutNameRejected(t *testing.T) {
	s := New(t.TempDir())
	src := writeSkill(t, "---\ndescription: no name here\n---\nbody\n")
	if _, err := s.Install(context.Background(), src, InstallOptions{}); err == nil {
		t.Error("expected error for a skill with no name")
	}
}
