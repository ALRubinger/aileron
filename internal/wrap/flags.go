package wrap

import (
	"bufio"
	"regexp"
	"strings"
)

// flagSpec is one parsed flag from a Cobra-style `--help` block. The
// wrap heuristic emits one [ParamSpec] per exposed flag and one
// argv-pattern token (or `[...]` optional group) per flag. Boolean
// flags and repeatable-array flags are intentionally not modeled at
// this layer — the parser skips them, so they never reach the
// emitter.
type flagSpec struct {
	// Name is the long-form flag name without the leading `--`
	// (e.g. "title"). Cobra's --help always lists the long form;
	// the shorthand (`-h, --help`) is dropped by the parser.
	Name string

	// Type is the Cobra-declared type token: "string", "int",
	// "int32", "int64", "float", "float32", "float64", "duration".
	// Empty when no type token appears on the row (Cobra-boolean
	// flags) — those are skipped by the parser and never reach the
	// emitter, so the emitter does not have to model the no-type
	// case.
	Type string

	// Description is the rest of the row after the type token,
	// trimmed of trailing markers like `(required)`.
	Description string

	// Required is true when the description carried the literal
	// `(required)` marker that Cobra appends for flags declared
	// with cmd.MarkFlagRequired.
	Required bool
}

// parseFlags reads a Cobra-style `--help` block and returns the
// flag list aggregated from the "Flags:" and "Global Flags:"
// sections. Both sections are emitted on every leaf — globals are
// inherited by every callsite of the binary, so an agent invoking
// the leaf may legitimately pass them.
//
// The parser is intentionally narrow: it recognizes Cobra's row
// shape (`-x, --name <type> <description>`) and skips lines that
// don't look like flag rows (section headers, blank lines, lines
// without a `--` token). Repeatable flags (`stringSlice`, `strings`,
// `stringArray`) and boolean flags (no type token) are dropped at
// parse time — the argv-pattern model can't express them in v1, so
// the emitter never has to filter again downstream.
func parseFlags(help string) []flagSpec {
	var out []flagSpec
	scanner := bufio.NewScanner(strings.NewReader(help))
	inFlagSection := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if isFlagSectionHeader(trimmed) {
			inFlagSection = true
			continue
		}
		if !inFlagSection {
			continue
		}
		// Non-flag-row lines end the section. Cobra terminates the
		// "Flags:" block with a blank line; a new section header
		// (e.g. "Global Flags:") immediately restarts the parser.
		if trimmed == "" {
			inFlagSection = false
			continue
		}
		// Flag rows are always indented in Cobra output. A non-
		// indented line (e.g. "Use ..." or "Available Commands:")
		// closes the section.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inFlagSection = false
			// Re-check whether this line is itself a section header
			// (e.g. immediately following "Flags:" with no blank).
			if isFlagSectionHeader(trimmed) {
				inFlagSection = true
			}
			continue
		}
		f, ok := parseFlagLine(trimmed)
		if !ok {
			continue
		}
		out = append(out, f)
	}
	return out
}

// isFlagSectionHeader recognizes Cobra's two flag-section headers.
// urfave/cli uses "Options:" with the same row shape; supported as
// a convenience even though PrintingPress's authoring convention
// targets Cobra.
func isFlagSectionHeader(line string) bool {
	low := strings.ToLower(strings.TrimSuffix(line, ":"))
	switch low {
	case "flags", "global flags", "options", "global options":
		return true
	}
	return false
}

// flagLineRe matches a Cobra flag row. The pattern captures:
//
//   - Optional shorthand block: `-x, ` (dropped).
//   - Long form: `--name`.
//   - Optional Cobra type token: `string`, `int`, `int32`, `int64`,
//     `float`, `float32`, `float64`, `duration`. Boolean flags have
//     no type token and the regex's type group stays empty.
//     Repeatable-array types (`strings`, `stringSlice`, `stringArray`,
//     `intSlice`) match here and are filtered out in parseFlagLine
//     so the v1 emitter never sees them.
//   - Description: the rest of the row.
//
// Cobra's column alignment puts at least two spaces between the
// type (or long-form for booleans) and the description; the regex
// allows one-or-more so handcrafted help text with single-space
// gaps still parses.
var flagLineRe = regexp.MustCompile(`^(?:-[a-zA-Z],\s+)?--([a-zA-Z][a-zA-Z0-9-]*)(?:\s+([a-zA-Z][a-zA-Z0-9]*))?\s+(.+?)\s*$`)

// supportedFlagType reports whether a Cobra type token can be
// represented in v1. String/numeric scalars are supported.
// Repeatable types are not — modeling them would require the argv
// pattern to repeat the flag N times, and the v1 grammar handles
// at most one token per flag.
func supportedFlagType(t string) bool {
	switch t {
	case "string", "int", "int32", "int64", "float", "float32", "float64", "duration":
		return true
	}
	return false
}

// parseFlagLine matches one Cobra flag row. Returns the parsed
// flagSpec on success, false when the line doesn't look like a
// flag row OR when the flag is not representable in v1 (booleans,
// repeatable arrays). Filtering at parse time keeps the emitter
// from having to special-case the wrap layer's coverage choices.
func parseFlagLine(line string) (flagSpec, bool) {
	m := flagLineRe.FindStringSubmatch(line)
	if m == nil {
		return flagSpec{}, false
	}
	name := m[1]
	typ := m[2]
	desc := strings.TrimSpace(m[3])

	// Skip `--help` — Cobra synthesizes it on every command. The
	// agent surface never wants to call --help via an operation;
	// it's a discovery affordance.
	if name == "help" {
		return flagSpec{}, false
	}
	if typ == "" {
		// Cobra-boolean: not representable as a `[--flag {flag}]`
		// optional group (the agent would have to supply "true" /
		// "false" / "" as input strings, which is not how boolean
		// flags work at the shell). Excluded in v1.
		return flagSpec{}, false
	}
	if !supportedFlagType(typ) {
		return flagSpec{}, false
	}

	required := false
	if strings.HasSuffix(desc, "(required)") {
		required = true
		desc = strings.TrimSpace(strings.TrimSuffix(desc, "(required)"))
	}

	return flagSpec{
		Name:        name,
		Type:        typ,
		Description: desc,
		Required:    required,
	}, true
}

// inputType maps a Cobra flag type to its action-manifest
// [action.Input] type. Centralized so the emitter and tests agree
// on the mapping without each holding its own switch.
func inputType(cobraType string) string {
	switch cobraType {
	case "string", "duration":
		return "string"
	case "int", "int32", "int64":
		return "integer"
	case "float", "float32", "float64":
		return "number"
	}
	return "string"
}
