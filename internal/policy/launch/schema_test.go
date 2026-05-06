package launch_test

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/policy/launch"
	"gopkg.in/yaml.v3"
)

func TestRule_UnmarshalYAML_InvalidNodeKind(t *testing.T) {
	// A rule must be a string or mapping; a sequence should produce an error.
	input := `
- - nested
  - sequence
`
	var rules []launch.Rule
	err := yaml.Unmarshal([]byte(input), &rules)
	if err == nil {
		t.Fatal("expected error for sequence node in rule position")
	}
}

func TestRule_UnmarshalYAML_StringForm(t *testing.T) {
	input := `
- "go test ./..."
- "go build"
`
	var rules []launch.Rule
	if err := yaml.Unmarshal([]byte(input), &rules); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if rules[0].Command != "go test ./..." {
		t.Errorf("rules[0].Command = %q, want 'go test ./...'", rules[0].Command)
	}
	if rules[1].Command != "go build" {
		t.Errorf("rules[1].Command = %q, want 'go build'", rules[1].Command)
	}
}

func TestRule_UnmarshalYAML_InvalidMappingField(t *testing.T) {
	// A mapping with an invalid type for a known field should produce
	// a decode error (the "decoding rule:" path).
	input := `
- command: [1, 2, 3]
`
	var rules []launch.Rule
	err := yaml.Unmarshal([]byte(input), &rules)
	if err == nil {
		t.Fatal("expected error for invalid field type in rule mapping")
	}
}

func TestRule_UnmarshalYAML_MappingForm(t *testing.T) {
	input := `
- command: "rm -rf *"
  description: "block recursive force delete"
`
	var rules []launch.Rule
	if err := yaml.Unmarshal([]byte(input), &rules); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Command != "rm -rf *" {
		t.Errorf("Command = %q, want 'rm -rf *'", rules[0].Command)
	}
	if rules[0].Description != "block recursive force delete" {
		t.Errorf("Description = %q, want 'block recursive force delete'", rules[0].Description)
	}
}
