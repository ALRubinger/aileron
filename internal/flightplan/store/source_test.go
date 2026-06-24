package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifySource(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		src  string
		want SourceKind
	}{
		{dir, SourceLocalPath},
		{"https://github.com/acme/skill.git", SourceGitURL},
		{"git@github.com:acme/skill.git", SourceGitURL},
		{"ssh://git@host/acme/skill", SourceGitURL},
		{"acme/weekly-digest", SourceSlug},
	}
	for _, tt := range tests {
		if got := classifySource(tt.src); got != tt.want {
			t.Errorf("classifySource(%q) = %v, want %v", tt.src, got, tt.want)
		}
	}
}

func TestLocalSourceDirFile(t *testing.T) {
	dir := writeSkill(t, instructionOnly)
	// Pointing at the SKILL.md file directly resolves to its directory.
	got, err := localSourceDir(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatalf("localSourceDir: %v", err)
	}
	if got != dir {
		t.Errorf("localSourceDir = %q, want %q", got, dir)
	}
}

func TestLocalSourceDirRejectsNonSkillFile(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localSourceDir(other); err == nil {
		t.Error("expected error for a non-SKILL.md file source")
	}
}

func TestLocalSourceDirMissingSkillMd(t *testing.T) {
	dir := t.TempDir() // empty dir, no SKILL.md
	if _, err := localSourceDir(dir); err == nil {
		t.Error("expected error for a directory with no SKILL.md")
	}
}

// TestInstallGitURLViaLocalBareRepo exercises the git-URL install path
// against a real local git repository (file:// clone), so no network is
// required. It proves the clone → parse → store write flow end to end.
func TestInstallGitURLViaLocalBareRepo(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "t@example.com")
	mustGit(t, repo, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(repo, "SKILL.md"), []byte(instructionOnly), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "SKILL.md")
	mustGit(t, repo, "commit", "-q", "-m", "init")

	s := New(t.TempDir())
	// Build a cross-platform file URL. On Windows a temp path is like
	// C:\...; the valid file URL form is file:///C:/... (forward slashes,
	// triple slash). filepath.ToSlash + a leading slash normalizes both.
	slashed := filepath.ToSlash(repo)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	url := "file://" + slashed
	res, err := s.Install(context.Background(), url, InstallOptions{})
	if err != nil {
		t.Fatalf("git-URL install: %v", err)
	}
	if res.Name != "rubber-duck" {
		t.Errorf("installed name = %q", res.Name)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "SKILL.md")); err != nil {
		t.Errorf("git-cloned skill not written: %v", err)
	}
	// Regression: the store must hold skill content only, never the clone's
	// .git directory.
	if _, err := os.Stat(filepath.Join(res.Dir, ".git")); !os.IsNotExist(err) {
		t.Errorf("install copied the clone's .git into the store (err=%v)", err)
	}
}

func TestInstallGitURLCloneFailureSurfaces(t *testing.T) {
	s := New(t.TempDir())
	// A git runner seam that fails proves clone errors surface as install
	// errors rather than being swallowed.
	failGit := func(ctx context.Context, args ...string) error {
		return errClone
	}
	_, err := s.Install(context.Background(), "https://example.invalid/x.git", InstallOptions{git: failGit})
	if err == nil {
		t.Error("expected clone failure to surface")
	}
}
