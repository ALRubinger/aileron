package wrap

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
)

// TestParsePositionals_OptionalSquareBrackets pins the canonical
// Cobra leaf shape `<binary> verb [POS] [flags]`. Square brackets
// mark the positional as optional — the parser must record it so
// the wrap emitter can wrap it in an `[{name}]` argv group.
func TestParsePositionals_OptionalSquareBrackets(t *testing.T) {
	help := `Run a SQL query against the local store.

Usage:
  linear-pp-cli sql [SQL] [flags]

Flags:
      --json   Output as JSON
`
	got := parsePositionals(help)
	if len(got) != 1 {
		t.Fatalf("got %d positionals, want 1: %+v", len(got), got)
	}
	if got[0].Name != "sql" {
		t.Errorf("name = %q, want sql", got[0].Name)
	}
	if got[0].Required {
		t.Errorf("required = true; square-bracket markers are optional")
	}
}

// TestParsePositionals_RequiredAngleBrackets covers the convention
// some CLIs use for required positionals (`<ARG>`). Required-ness
// flows through to the action input's `required` field.
func TestParsePositionals_RequiredAngleBrackets(t *testing.T) {
	help := `Get one issue by id.

Usage:
  linear-pp-cli issue get <ID> [flags]
`
	got := parsePositionals(help)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Name != "id" {
		t.Errorf("name = %q, want id", got[0].Name)
	}
	if !got[0].Required {
		t.Errorf("required = false; angle-bracket markers are required")
	}
}

// TestParsePositionals_FiltersReservedMarkers ensures `[flags]`,
// `[command]`, `[options]`, and `[args]` are not mistaken for
// positional arguments. These are Cobra/urfave structural
// markers, not named positional slots.
func TestParsePositionals_FiltersReservedMarkers(t *testing.T) {
	help := `Usage:
  cmd verb [flags] [options] [command] [args]
`
	got := parsePositionals(help)
	if len(got) != 0 {
		t.Errorf("got %+v; reserved markers should not become positionals", got)
	}
}

// TestParsePositionals_MultiplePositionalsPreserveOrder confirms
// the argv-render order matches the Usage-line order (Cobra
// preserves command-line position via the marker order).
func TestParsePositionals_MultiplePositionalsPreserveOrder(t *testing.T) {
	help := `Move a file.

Usage:
  cmd move <SOURCE> <DEST> [flags]
`
	got := parsePositionals(help)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "source" || got[1].Name != "dest" {
		t.Errorf("order/names wrong: %+v", got)
	}
}

// TestParsePositionals_SkipsCommandLine confirms the
// `<binary> verb [command]` Usage line — Cobra emits this on
// namespace commands to advertise the subcommand pivot — does
// not contribute a `command` positional.
func TestParsePositionals_SkipsCommandLine(t *testing.T) {
	help := `Usage:
  cmd verb [SQL] [flags]
  cmd verb [command]
`
	got := parsePositionals(help)
	if len(got) != 1 || got[0].Name != "sql" {
		t.Errorf("got %+v; should keep [SQL] and skip the [command] line", got)
	}
}

// TestParsePositionals_VariadicDropped covers the v1 limitation:
// `[ARGS...]` / `<FILES...>` aren't representable in the argv-
// pattern grammar (single placeholder, fixed token count). The
// parser drops them so the emitter never sees them. Tracked for
// the same follow-up that covers repeatable flag arrays.
func TestParsePositionals_VariadicDropped(t *testing.T) {
	help := `Usage:
  cmd batch <FILES...> [flags]
`
	got := parsePositionals(help)
	if len(got) != 0 {
		t.Errorf("got %+v; variadic positionals should be dropped in v1", got)
	}
}

// TestParsePositionals_DashedMarkerNamesNormalized confirms the
// same dash-to-underscore translation applied to flag names is
// applied to positional marker names too (`<FILE-PATH>` →
// `file_path`). Keeps the action-input regex satisfied.
func TestParsePositionals_DashedMarkerNamesNormalized(t *testing.T) {
	help := `Usage:
  cmd open <FILE-PATH> [flags]
`
	got := parsePositionals(help)
	if len(got) != 1 || got[0].Name != "file_path" {
		t.Errorf("got %+v; want file_path (dash normalized to underscore)", got)
	}
}

// TestParsePositionals_LowercaseMarkerNames accepts CLIs that
// follow a `[file-path]` lowercase convention rather than the
// uppercase Cobra default.
func TestParsePositionals_LowercaseMarkerNames(t *testing.T) {
	help := `Usage:
  cmd open [file-path] [flags]
`
	got := parsePositionals(help)
	if len(got) != 1 || got[0].Name != "file_path" {
		t.Errorf("got %+v; want file_path", got)
	}
}

// TestParsePositionals_LeadingDigitDropped covers the
// regex-compliance fallback: a positional whose normalized name
// starts with a digit (e.g. `[2FA_CODE]`) can't be an action
// input — the validator's `^[a-z][a-z0-9_]*$` rejects leading
// digits. The parser drops these rather than emit an action.md
// that would fail validation downstream.
func TestParsePositionals_LeadingDigitDropped(t *testing.T) {
	help := `Usage:
  cmd login <2FA_CODE> [flags]
`
	got := parsePositionals(help)
	if len(got) != 0 {
		t.Errorf("got %+v; leading-digit names should drop", got)
	}
}

// TestParsePositionals_OverloadedUsageLinesFirstWins covers the
// edge case where Cobra emits multiple Usage lines (one with
// positionals, one without). The parser takes the first leaf-
// shape line that yields positionals so emission stays
// deterministic.
func TestParsePositionals_OverloadedUsageLinesFirstWins(t *testing.T) {
	help := `Usage:
  cmd issues [ID] [flags]
  cmd issues --interactive
`
	got := parsePositionals(help)
	if len(got) != 1 || got[0].Name != "id" {
		t.Errorf("got %+v; want one positional from first leaf line", got)
	}
}

// TestParsePositionals_NoUsageBlockReturnsNil tolerates --help
// output that lacks a Usage block (rare but possible). No
// positionals → no error.
func TestParsePositionals_NoUsageBlockReturnsNil(t *testing.T) {
	help := `Just some prose.

Flags:
  --json   Output as JSON
`
	got := parsePositionals(help)
	if len(got) != 0 {
		t.Errorf("got %+v; want none", got)
	}
}

// --- buildLeafSpec integration ---

// TestBuildLeafSpec_PositionalRequiredRendersInline confirms a
// required positional becomes a bare `{name}` placeholder in the
// argv (no brackets), positioned before any flags. The action
// input is marked required.
func TestBuildLeafSpec_PositionalRequiredRendersInline(t *testing.T) {
	leaf := buildLeafSpec(
		"linear-pp-cli",
		[]string{"issue", "get"},
		"Get one issue",
		[]positionalSpec{{Name: "id", Required: true}},
		nil,
	)
	wantArgv := "linear-pp-cli issue get {id}"
	if leaf.Argv != wantArgv {
		t.Errorf("argv = %q\n  want = %q", leaf.Argv, wantArgv)
	}
	if len(leaf.Params) != 1 || leaf.Params[0].Name != "id" || !leaf.Params[0].Required {
		t.Errorf("params = %+v; want id required=true", leaf.Params)
	}
}

// TestBuildLeafSpec_PositionalOptionalWrapsInOptionalGroup checks
// that `[ARG]`-shaped (optional) positionals end up in the same
// `[...]` optional-group syntax used for optional flags. Lets the
// agent omit the positional and Cobra still accepts the bare
// invocation.
func TestBuildLeafSpec_PositionalOptionalWrapsInOptionalGroup(t *testing.T) {
	leaf := buildLeafSpec(
		"linear-pp-cli",
		[]string{"sql"},
		"Run SQL",
		[]positionalSpec{{Name: "sql", Required: false}},
		[]flagSpec{{Name: "rate-limit", Type: "float", Required: false}},
	)
	wantArgv := "linear-pp-cli sql [{sql}] [--rate-limit {rate_limit}]"
	if leaf.Argv != wantArgv {
		t.Errorf("argv = %q\n  want = %q", leaf.Argv, wantArgv)
	}
	// Both inputs surface as optional.
	for _, p := range leaf.Params {
		if p.Required {
			t.Errorf("param %q should be optional", p.Name)
		}
	}
}

// TestBuildLeafSpec_PositionalsBeforeFlags pins the argv ordering
// contract: Cobra expects positionals before flags on the command
// line. The emitter must follow that order so the substituted
// argv parses correctly when the wrapped CLI re-tokenizes it.
func TestBuildLeafSpec_PositionalsBeforeFlags(t *testing.T) {
	leaf := buildLeafSpec(
		"mycli",
		[]string{"do"},
		"Do a thing",
		[]positionalSpec{{Name: "thing", Required: true}},
		[]flagSpec{
			{Name: "force", Type: "string", Required: true},
			{Name: "config", Type: "string", Required: false},
		},
	)
	wantArgv := "mycli do {thing} --force {force} [--config {config}]"
	if leaf.Argv != wantArgv {
		t.Errorf("argv = %q\n  want = %q", leaf.Argv, wantArgv)
	}
}

// TestBuildLeafSpec_PositionalOutputValidates is the boundary
// check: an emitted action.md with positional inputs must parse
// and validate against internal/action's loader. Catches the same
// shape of bug that PR #800 fixed for dashed flag names.
func TestBuildLeafSpec_PositionalOutputValidates(t *testing.T) {
	leaf := buildLeafSpec(
		"linear-pp-cli",
		[]string{"sql"},
		"Run SQL against the local store",
		[]positionalSpec{{Name: "sql", Required: false}},
		nil,
	)
	spec := &Spec{
		Connector:   ConnectorSpec{Name: "local://user/linear", Version: "0.0.1"},
		Program:     ProgramSpec{Path: "/usr/local/bin/linear-pp-cli"},
		Subcommands: []SubcommandSpec{leaf},
	}
	manifest := BuildManifest(spec)
	md := RenderActionMD(spec, leaf, manifest, "sha256:bound-at-install")
	parsed, err := action.Parse(ActionFileName(spec, leaf), []byte(md))
	if err != nil {
		t.Fatalf("emitted action.md does not parse: %v\n%s", err, md)
	}
	if err := action.Validate(parsed, ActionFileName(spec, leaf)); err != nil {
		t.Fatalf("emitted action.md fails validation: %v\n%s", err, md)
	}
}
