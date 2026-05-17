package wrap

import (
	"bufio"
	"regexp"
	"strings"
)

// positionalSpec is one parsed positional argument from a
// Cobra-style `--help` Usage block. The wrap heuristic emits one
// [ParamSpec] per positional and one argv-pattern token (or
// `[{name}]` optional group for Cobra's `[ARG]` shape) per
// positional.
//
// Variadic positionals (`[ARGS...]` / `<ARGS...>`) are not modeled
// in v1 — the argv-pattern grammar matches token-for-token and
// can't expand a single placeholder into many output tokens. The
// parser skips variadic forms so the emitter never sees them.
type positionalSpec struct {
	// Name is the lowercased + underscore-normalized identifier
	// derived from the Cobra positional marker. `[SQL]` → `sql`;
	// `<FILE-PATH>` → `file_path`. Must satisfy the action input
	// regex `^[a-z][a-z0-9_]*$` — positionals whose normalized
	// name doesn't (e.g. leading digit) are dropped at parse time.
	Name string

	// Required is true when the Cobra marker used angle brackets
	// (`<ARG>`); false for square brackets (`[ARG]`). Mirrors
	// Cobra's convention — square brackets are "optional," angle
	// brackets are "required." CLIs that don't follow the
	// convention (some use only `[ARG]` regardless of
	// requiredness) end up with everything optional; the agent can
	// still pass values but its tool schema won't mark them
	// required. Acceptable v1 trade-off.
	Required bool
}

// parsePositionals reads a Cobra-style `--help` block and extracts
// the leaf command's positional arguments from its Usage section.
//
// Cobra emits one or more Usage lines after the "Usage:" header.
// For a leaf, the line takes the shape:
//
//	binary <verb...> <REQ_POS> [OPT_POS] [flags]
//
// Lines whose tail token is `[command]` describe the namespace
// invocation form for a non-leaf command and are ignored. Tokens
// inside square or angle brackets that match reserved Cobra
// markers (`flags`, `command`, `options`, `args` —
// case-insensitive) are not positionals and are filtered out.
// Everything else inside `[...]` or `<...>` is treated as a
// positional with the appropriate required-ness.
//
// The order in the returned slice matches the order positionals
// appear in the first parseable Usage line; if multiple lines
// declare different positional sets (Cobra allows this for
// commands that overload), the first one wins and the rest are
// ignored. Duplicates across lines are not collected — each
// positional appears once.
func parsePositionals(help string) []positionalSpec {
	scanner := bufio.NewScanner(strings.NewReader(help))
	inUsage := false
	var out []positionalSpec
	seen := map[string]bool{}
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if !inUsage {
			if isUsageHeader(trimmed) {
				inUsage = true
			}
			continue
		}
		// Blank line ends the section.
		if trimmed == "" {
			if len(out) > 0 {
				return out
			}
			continue
		}
		// Non-indented line means we've left the Usage block.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			return out
		}
		// The `<binary> <verbs...> [command]` line describes the
		// namespace-pivot invocation form; the leaf positional
		// shape lives on a sibling line, so skip this one.
		if strings.HasSuffix(trimmed, "[command]") {
			continue
		}
		for _, p := range extractPositionalsFromUsageLine(trimmed) {
			if seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			out = append(out, p)
		}
		// One leaf-shape line is enough; subsequent overload
		// lines would only confuse the emitter (e.g. a leaf that
		// accepts EITHER one positional OR zero, depending on
		// flags). Bail after the first line that yielded
		// positionals so the wrap stays deterministic.
		if len(out) > 0 {
			return out
		}
	}
	return out
}

// isUsageHeader recognizes the "Usage:" section header that Cobra
// emits at the top of any --help block. urfave/cli uses "USAGE:"
// (uppercase) with a similar single-line shape; supported for
// parity with parseSubcommands' urfave handling.
func isUsageHeader(line string) bool {
	low := strings.ToLower(strings.TrimSuffix(line, ":"))
	return low == "usage"
}

// usageMarkerRe matches one `[...]` or `<...>` token. The body is
// captured for filtering. Variadic markers (trailing `...` inside
// the brackets) are recognized but the body matcher excludes them
// — see extractPositionalsFromUsageLine.
var usageMarkerRe = regexp.MustCompile(`(?:\[([^\[\]]+)\]|<([^<>]+)>)`)

// reservedUsageMarkers names the Cobra/urfave-style tokens that
// appear inside `[...]` in a Usage line but are NOT positional
// args. They describe slots for flags / subcommands / option
// blocks, not named positional values.
var reservedUsageMarkers = map[string]bool{
	"flags":   true,
	"command": true,
	"options": true,
	"args":    true,
}

// extractPositionalsFromUsageLine returns the positional specs
// declared on a single Usage line, in the order they appear. The
// caller filters duplicates and limits to the first leaf-shape
// line.
func extractPositionalsFromUsageLine(line string) []positionalSpec {
	matches := usageMarkerRe.FindAllStringSubmatch(line, -1)
	out := make([]positionalSpec, 0, len(matches))
	for _, m := range matches {
		var body string
		var required bool
		switch {
		case m[1] != "":
			body = m[1]
			required = false
		case m[2] != "":
			body = m[2]
			required = true
		default:
			continue
		}
		// Variadic forms (`[ARGS...]`, `<FILES...>`) aren't
		// representable in the v1 argv-pattern grammar — drop
		// them so the emitter doesn't get a placeholder it can't
		// honor at substitution time. Tracked for the same
		// follow-up that covers repeatable flag arrays.
		if strings.HasSuffix(body, "...") {
			continue
		}
		// Reserved markers are not positionals — they're Cobra's
		// shorthand for "flags can go here," etc.
		if reservedUsageMarkers[strings.ToLower(body)] {
			continue
		}
		name := positionalInputName(body)
		if name == "" {
			continue
		}
		out = append(out, positionalSpec{Name: name, Required: required})
	}
	return out
}

// positionalInputName normalizes a Cobra positional marker into
// an action-manifest input name. Lowercases and translates dashes
// to underscores to satisfy the input regex `^[a-z][a-z0-9_]*$`
// (`internal/action/validate.go`). Returns the empty string when
// the normalized result wouldn't validate (e.g. leading digit,
// empty body, non-ASCII chars) — callers drop those positionals
// rather than emit an action.md that would fail validation.
func positionalInputName(marker string) string {
	name := strings.ToLower(strings.ReplaceAll(marker, "-", "_"))
	if !positionalNameRe.MatchString(name) {
		return ""
	}
	return name
}

// positionalNameRe matches the action-input name shape (same as
// the validator's `inputNameRe`). Pinned locally so the emitter
// can drop positionals before they reach `action.Validate`.
var positionalNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
