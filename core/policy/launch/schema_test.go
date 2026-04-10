package launch_test

import (
	"testing"

	"github.com/ALRubinger/aileron/core/policy/launch"
	"gopkg.in/yaml.v3"
)

func TestQuietHours_ParseFromYAML(t *testing.T) {
	pf, err := launch.Load(testdataPath("quiet_hours.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if pf.Notifications == nil {
		t.Fatal("expected Notifications to be non-nil")
	}
	qh := pf.Notifications.QuietHours
	if qh == nil {
		t.Fatal("expected QuietHours to be non-nil")
	}
	if qh.Start != "22:00" {
		t.Errorf("Start = %q, want '22:00'", qh.Start)
	}
	if qh.End != "08:00" {
		t.Errorf("End = %q, want '08:00'", qh.End)
	}
	if qh.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q, want 'America/New_York'", qh.Timezone)
	}
}

func TestQuietHours_ChannelPriority(t *testing.T) {
	pf, err := launch.Load(testdataPath("quiet_hours.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if pf.Notifications == nil || pf.Notifications.Slack == nil {
		t.Fatal("expected Slack notifications to be non-nil")
	}
	channels := pf.Notifications.Slack.Channels
	if len(channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(channels))
	}
	if channels[0].Priority != "high" {
		t.Errorf("channels[0].Priority = %q, want 'high'", channels[0].Priority)
	}
	if channels[1].Priority != "normal" {
		t.Errorf("channels[1].Priority = %q, want 'normal'", channels[1].Priority)
	}
}

func TestQuietHours_OmittedIsNil(t *testing.T) {
	pf, err := launch.Load(testdataPath("basic.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if pf.Notifications != nil {
		t.Error("expected Notifications to be nil for basic.yaml")
	}
}

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
