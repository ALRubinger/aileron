package launch

import (
	"os"
	"path/filepath"
	"testing"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// installSkill writes a minimal SKILL.md under dir/<name>/ so the store's
// List sees it as an installed skill.
func installSkill(t *testing.T, storeDir, name string) {
	t.Helper()
	skillDir := filepath.Join(storeDir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: "+name+"\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillsAgent is a minimal Agent whose only relevant behavior is its
// SkillsPath; the appendSkillsMount logic depends on nothing else.
type skillsAgent struct {
	name string
	path string
}

func (a skillsAgent) Name() string           { return a.name }
func (a skillsAgent) BinaryNames() []string  { return []string{a.name} }
func (a skillsAgent) Args(Mode) []string     { return nil }
func (a skillsAgent) Env() map[string]string { return nil }
func (a skillsAgent) LLMEndpointEnv() string { return "" }
func (a skillsAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (a skillsAgent) AuthSpec() AuthSpec { return AuthSpec{} }
func (a skillsAgent) SkillsPath() string { return a.path }

func TestSkillsStoreNonEmpty(t *testing.T) {
	dir := t.TempDir()
	if skillsStoreNonEmpty(dir) {
		t.Error("empty store should report not-non-empty")
	}
	installSkill(t, dir, "alpha")
	if !skillsStoreNonEmpty(dir) {
		t.Error("store with a skill should report non-empty")
	}
}

func TestAppendSkillsMount_SkippedWhenNoPath(t *testing.T) {
	dir := t.TempDir()
	installSkill(t, dir, "alpha")
	agent := skillsAgent{name: "noskills", path: ""}
	mounts, err := appendSkillsMount(nil, agent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Errorf("agent with no skills path must add no mount, got %v", mounts)
	}
}

func TestAppendSkillsMount_SkippedWhenStoreEmpty(t *testing.T) {
	dir := t.TempDir() // no skills installed
	agent := skillsAgent{name: "claude", path: "/home/agent/.claude/skills"}
	mounts, err := appendSkillsMount(nil, agent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Errorf("empty store must add no mount, got %v", mounts)
	}
}

func TestAppendSkillsMount_ReadOnlyBindWhenNotNested(t *testing.T) {
	dir := t.TempDir()
	installSkill(t, dir, "alpha")
	agent := skillsAgent{name: "codex", path: "/home/agent/.codex/skills"}

	// Existing mounts that do NOT enclose the skills target.
	existing := []sandboxcontainer.Volume{
		{Source: "/host/auth.json", Target: "/home/agent/.codex/auth.json", ReadOnly: false},
	}
	mounts, err := appendSkillsMount(existing, agent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 {
		t.Fatalf("expected the skills mount appended, got %v", mounts)
	}
	got := mounts[1]
	if got.Target != "/home/agent/.codex/skills" || got.Source != dir || !got.ReadOnly {
		t.Errorf("skills mount = %+v, want read-only %s -> /home/agent/.codex/skills", got, dir)
	}
}

func TestAppendSkillsMount_CopiesIntoEnclosingDirMount(t *testing.T) {
	dir := t.TempDir()
	installSkill(t, dir, "alpha")
	agent := skillsAgent{name: "claude", path: "/home/agent/.claude/skills"}

	// Claude's AuthSpec mounts /home/agent/.claude as a writable directory.
	// The skills target nests inside it, so the store content must be
	// copied into the parent's host source rather than added as a nested
	// bind (issue #1143 hazard avoidance).
	authHostDir := t.TempDir()
	existing := []sandboxcontainer.Volume{
		{Source: authHostDir, Target: "/home/agent/.claude", ReadOnly: false},
	}
	mounts, err := appendSkillsMount(existing, agent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("nested skills must not add a new mount, got %v", mounts)
	}
	// The store content landed under the parent mount's host source at the
	// relative subpath (skills/).
	copied := filepath.Join(authHostDir, "skills", "alpha", "SKILL.md")
	if _, err := os.Stat(copied); err != nil {
		t.Errorf("skill content not copied into enclosing mount: %v", err)
	}
}

func TestAppendSkillsMount_SingleReadOnlyBindForGroundedAgent(t *testing.T) {
	// With no pre-existing mounts, a grounded agent gets exactly one
	// read-only skills bind at its skills path. (The host launch path never
	// calls this helper — projection is a sandbox-mode behavior — so the
	// agent on the host reads its own native skills dir untouched.)
	dir := t.TempDir()
	installSkill(t, dir, "alpha")
	agent := skillsAgent{name: "claude", path: "/home/agent/.claude/skills"}
	mounts, err := appendSkillsMount(nil, agent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Target != "/home/agent/.claude/skills" {
		t.Errorf("expected a single read-only skills mount, got %v", mounts)
	}
}
