package wrap

import (
	"testing"
)

// TestParseFlags_CobraIssuesCreateFixture is the load-bearing
// regression: parse the actual --help shape `linear-pp-cli issues
// create` emits. Title and team are required; the rest are
// optional. Boolean and array-typed globals must be silently
// dropped — v1 can't represent them in the argv pattern.
func TestParseFlags_CobraIssuesCreateFixture(t *testing.T) {
	help := `Create a Linear issue.

Usage:
  linear-pp-cli issues create [flags]

Flags:
      --assignee string      Assignee user UUID
      --db string            Database path (for team-key resolution and pp_created ledger)
      --description string   Issue description (markdown)
  -h, --help                 help for create
      --label strings        Label UUIDs (repeatable)
      --priority int         Priority: 1=Urgent, 2=High, 3=Medium, 4=Low (0=None)
      --project string       Project UUID
      --state string         Workflow state UUID
      --team string          Team key (e.g. ENG) or team UUID (required)
      --title string         Issue title (required)

Global Flags:
      --agent                Set all agent-friendly defaults
      --json                 Output as JSON
      --config string        Config file path
`
	got := parseFlags(help)
	want := map[string]struct {
		typ      string
		required bool
	}{
		"assignee":    {"string", false},
		"db":          {"string", false},
		"description": {"string", false},
		"priority":    {"int", false},
		"project":     {"string", false},
		"state":       {"string", false},
		"team":        {"string", true},
		"title":       {"string", true},
		"config":      {"string", false},
	}
	if len(got) != len(want) {
		t.Errorf("got %d flags, want %d: %+v", len(got), len(want), got)
	}
	for _, f := range got {
		exp, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected flag %q (boolean/array flags should drop)", f.Name)
			continue
		}
		if f.Type != exp.typ {
			t.Errorf("%s: type = %q, want %q", f.Name, f.Type, exp.typ)
		}
		if f.Required != exp.required {
			t.Errorf("%s: required = %v, want %v", f.Name, f.Required, exp.required)
		}
	}
}

// TestParseFlags_SkipsBooleanFlags pins the v1 contract: boolean
// flags (those without a Cobra type token) are not representable
// in the argv pattern, so the parser drops them. A future PR can
// add `[?{flag}:--flag]` syntax (or similar) and exercise this
// path positively.
func TestParseFlags_SkipsBooleanFlags(t *testing.T) {
	help := `Flags:
      --agent           Set all agent-friendly defaults
      --json            Output as JSON
      --csv             Output as CSV
  -y, --yes             Skip the confirmation prompt
      --title string    Issue title
`
	got := parseFlags(help)
	if len(got) != 1 {
		t.Fatalf("got %d flags, want 1 (only --title is non-boolean): %+v", len(got), got)
	}
	if got[0].Name != "title" {
		t.Errorf("Name = %q, want title", got[0].Name)
	}
}

// TestParseFlags_SkipsRepeatableArrayFlags pins the v1 contract:
// `strings`, `stringSlice`, `stringArray`, `intSlice` flags need
// argv repetition (--label foo --label bar), which the manifest
// argv pattern can't model in a single optional group. Dropped at
// parse time so the emitter doesn't have to special-case the
// wrap-layer coverage choices downstream.
func TestParseFlags_SkipsRepeatableArrayFlags(t *testing.T) {
	help := `Flags:
      --label strings       Label UUIDs (repeatable)
      --tag stringSlice     Tag names
      --port intSlice       Ports
      --title string        Issue title
`
	got := parseFlags(help)
	if len(got) != 1 || got[0].Name != "title" {
		t.Errorf("got %+v, want only --title", got)
	}
}

// TestParseFlags_GlobalFlagsBlock confirms the parser walks both
// "Flags:" and "Global Flags:" sections and merges the entries.
// Globals are inherited by every leaf, so an agent invoking the
// leaf may legitimately pass them.
func TestParseFlags_GlobalFlagsBlock(t *testing.T) {
	help := `Flags:
      --title string    Issue title (required)

Global Flags:
      --config string   Config file path
`
	got := parseFlags(help)
	if len(got) != 2 {
		t.Fatalf("got %d flags, want 2: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f.Name] = true
	}
	if !seen["title"] || !seen["config"] {
		t.Errorf("missing flags: got %+v", seen)
	}
}

// TestParseFlags_RequiredMarkerStrippedFromDescription confirms
// the parser pulls `(required)` off the description text so the
// emitted action.md doesn't surface the marker to the LLM (the
// required-ness is already encoded in the input's `required`
// field).
func TestParseFlags_RequiredMarkerStrippedFromDescription(t *testing.T) {
	help := `Flags:
      --title string    Issue title (required)
`
	got := parseFlags(help)
	if len(got) != 1 {
		t.Fatalf("got %d flags, want 1", len(got))
	}
	if got[0].Description != "Issue title" {
		t.Errorf("Description = %q, want %q", got[0].Description, "Issue title")
	}
	if !got[0].Required {
		t.Error("Required = false, want true")
	}
}

// TestParseFlags_IntegerType verifies the int type token maps to
// the action manifest's "integer" input type (JSON Schema spelling).
func TestParseFlags_IntegerType(t *testing.T) {
	help := `Flags:
      --priority int   Priority: 1=Urgent
`
	got := parseFlags(help)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Type != "int" {
		t.Errorf("Cobra Type = %q, want int", got[0].Type)
	}
	if inputType(got[0].Type) != "integer" {
		t.Errorf("inputType(int) = %q, want integer", inputType(got[0].Type))
	}
}

// TestParseFlags_HelpFlagDropped confirms `--help` is filtered.
// Cobra synthesizes it on every command, but it isn't an
// operation an agent should ever call through the wrap.
func TestParseFlags_HelpFlagDropped(t *testing.T) {
	help := `Flags:
  -h, --help              help for create
      --title string      Issue title
`
	got := parseFlags(help)
	if len(got) != 1 || got[0].Name != "title" {
		t.Errorf("got %+v, want only --title (no --help)", got)
	}
}

// TestParseFlags_NoFlagSection returns no flags rather than
// erroring. CLIs whose --help has no Flags: block (extremely rare,
// but possible for unusually-shaped output) just produce a
// flagless leaf.
func TestParseFlags_NoFlagSection(t *testing.T) {
	help := `Usage:
  thing run
`
	got := parseFlags(help)
	if len(got) != 0 {
		t.Errorf("got %d flags, want 0", len(got))
	}
}

func TestInputType_AllSupportedCobraTypes(t *testing.T) {
	cases := map[string]string{
		"string":   "string",
		"duration": "string",
		"int":      "integer",
		"int32":    "integer",
		"int64":    "integer",
		"float":    "number",
		"float32":  "number",
		"float64":  "number",
		"":         "string", // fallback for unknown
	}
	for in, want := range cases {
		got := inputType(in)
		if got != want {
			t.Errorf("inputType(%q) = %q, want %q", in, got, want)
		}
	}
}
