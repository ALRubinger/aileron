package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleSkill loads the committed, schema-valid worked example. Using the
// canonical example as the valid-path fixture keeps the test honest: it
// exercises a manifest that the normative schema actually accepts, rather
// than a hand-rolled minimal block that could drift from the contract.
func exampleSkill(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	return raw
}

// instructionOnlySkill carries no aileron block. It is a valid skill.
const instructionOnlySkill = `---
name: rubber-duck
description: A skill with only instructions, no Aileron actions.
---

# Rubber Duck

Explain your problem out loud.
`

func TestParseInstructionOnly(t *testing.T) {
	m, err := Parse([]byte(instructionOnlySkill))
	if err != nil {
		t.Fatalf("instruction-only skill must parse: %v", err)
	}
	if !m.InstructionOnly {
		t.Error("expected InstructionOnly true for a skill with no aileron block")
	}
	if m.Name != "rubber-duck" {
		t.Errorf("name = %q, want rubber-duck", m.Name)
	}
	if len(m.Refs()) != 0 {
		t.Errorf("instruction-only Refs() = %v, want empty", m.Refs())
	}
	if !strings.Contains(m.Body, "Explain your problem out loud") {
		t.Errorf("body not captured: %q", m.Body)
	}
}

func TestParseValidCredentialed(t *testing.T) {
	m, err := Parse(exampleSkill(t))
	if err != nil {
		t.Fatalf("valid credentialed skill must parse: %v", err)
	}
	if m.InstructionOnly {
		t.Error("expected InstructionOnly false for a skill with an aileron block")
	}
	if m.Name != "weekly-metrics-digest" {
		t.Errorf("name = %q", m.Name)
	}
	want := []string{"aileron:metrics.query_series", "aileron:tracker.create_issue"}
	got := m.Refs()
	if len(got) != len(want) {
		t.Fatalf("Refs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Refs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if m.Aileron.SchemaVersion != "aileron.flightplan.v1" {
		t.Errorf("schemaVersion = %q", m.Aileron.SchemaVersion)
	}
	// The trust contract must carry only declarative kind/placement, never
	// a credential value. Assert no secret-shaped key leaked into the
	// parsed contract.
	tc := m.Aileron.Requires.Actions[0].TrustContract
	cred, ok := tc["credential"].(map[string]any)
	if !ok {
		t.Fatalf("trustContract.credential missing or wrong type: %T", tc["credential"])
	}
	for _, forbidden := range []string{"value", "secret", "token", "password"} {
		if _, present := cred[forbidden]; present {
			t.Errorf("trust contract leaked a credential value under %q", forbidden)
		}
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	_, err := Parse([]byte("# Just a heading\n\nNo frontmatter here.\n"))
	if !errors.Is(err, ErrNoFrontmatter) {
		t.Errorf("expected ErrNoFrontmatter, got %v", err)
	}
}

func TestParseThematicBreakNotFence(t *testing.T) {
	// A body line that is a thematic break (---) inside the markdown must
	// not be mistaken for the closing frontmatter fence.
	doc := "---\nname: x\ndescription: y\n---\n\nIntro\n\n---\n\nMore body.\n"
	m, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(m.Body, "More body.") {
		t.Errorf("body should include the thematic break and what follows: %q", m.Body)
	}
}
