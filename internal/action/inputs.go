package action

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// InputField is one rendered entry on the approval surface that
// describes a call-time argument the agent passed into a gated action.
// Mirrors [PreviewField] structurally so the approval card can render
// inputs and preview fields with one component, but lives behind its
// own type to preserve the semantic distinction: a [PreviewField]
// summarizes external state fetched by the runtime (per ADR-0016), an
// [InputField] summarizes what the agent is about to write.
type InputField struct {
	// Label is the user-facing label the approval surface renders
	// (e.g. "To"). Falls back to the input's bare name when the
	// manifest declared no `label`.
	Label string

	// Value is the stringified scalar from the agent's args. Empty
	// when Missing is true.
	Value string

	// Missing is true when the manifest declared the input as
	// required and the agent supplied no value for it. The UI
	// surfaces these as "n/a", matching the [PreviewField] convention,
	// so the user sees what the agent forgot rather than the row
	// being silently absent.
	Missing bool

	// Multiline is the presentation hint declared on the input. True
	// renders as a scrollable blockquote below the inline rows;
	// false renders as a single-line `key: value` row.
	Multiline bool
}

// BuildInputFields projects an agent's call-time args through a
// manifest's [[inputs]] declarations into the rendered shape the
// approval card consumes. Walks `Inputs` in declared order (matching
// the convention `PreviewPolicy.RenderOrder` follows for previews) so
// the user sees fields in the manifest author's intended order rather
// than Go's randomized map iteration order. Returns nil for a nil
// manifest so callers can pass straight through without a surrounding
// guard.
//
// Per the auto-render contract (the ADR-0003 amendment introducing
// `label` and `multiline`): every action with a parseable manifest
// gets a labeled-fields rendering, with the input's bare name acting
// as the label when no `label` is declared. The user never falls back
// to a raw-JSON-only view as long as the manifest has declared
// `[[inputs]]`.
//
// Args the agent supplied that don't correspond to any declared input
// are appended at the end (sorted by key) with `label = name` and
// `multiline = false`. This guarantees the user always sees everything
// the agent passed — no silent dropping of off-manifest values.
//
// Missing required inputs surface as `Missing = true` with an empty
// value, matching the preview convention so the renderer can route
// both through the same "n/a" affordance. Missing optional inputs
// (required=false) are omitted entirely.
func BuildInputFields(m *Manifest, args map[string]any) []InputField {
	if m == nil {
		return nil
	}
	declared := make(map[string]bool, len(m.Inputs))
	out := make([]InputField, 0, len(m.Inputs)+len(args))
	for _, in := range m.Inputs {
		declared[in.Name] = true
		label := in.Label
		if label == "" {
			label = in.Name
		}
		raw, ok := args[in.Name]
		if !ok || raw == nil {
			if in.IsRequired() {
				out = append(out, InputField{
					Label:     label,
					Missing:   true,
					Multiline: in.Multiline,
				})
			}
			// Optional unsupplied inputs are omitted — there is no
			// useful "n/a" affordance for "the agent chose not to
			// pass this optional field".
			continue
		}
		// Structured input types (array, object) render their value as
		// a pretty-printed JSON block. They are forced multiline so the
		// approval surface routes them through the scrollable affordance
		// rather than truncating to a single row. The validator forbids
		// `multiline = true` on non-string declarations, so authors
		// cannot have set this themselves — the projection layer makes
		// the rendering decision based on the declared type.
		multiline := in.Multiline
		if in.Type == "array" || in.Type == "object" {
			multiline = true
		}
		out = append(out, InputField{
			Label:     label,
			Value:     stringifyScalar(raw),
			Multiline: multiline,
		})
	}
	// Append any extras the agent passed that aren't declared on the
	// manifest. Sorted for determinism — the user-facing surface
	// should be stable across requests with the same args.
	extras := make([]string, 0)
	for k := range args {
		if declared[k] {
			continue
		}
		extras = append(extras, k)
	}
	sort.Strings(extras)
	for _, k := range extras {
		// Skip nil extras — an undeclared arg whose value the agent
		// passed as null is noise on the approval surface, not a
		// "missing required field" we'd want to surface as "n/a".
		if args[k] == nil {
			continue
		}
		// Detect structured values on undeclared extras: a stray map
		// or slice the agent passed off-manifest still wants the
		// multi-line affordance so the user can read the payload.
		multiline := false
		switch args[k].(type) {
		case map[string]any, []any:
			multiline = true
		}
		out = append(out, InputField{
			Label:     k,
			Value:     stringifyScalar(args[k]),
			Multiline: multiline,
		})
	}
	return out
}

// stringifyScalar renders an arg value as a string for the approval
// surface. v1 declared input types are string|integer|number|boolean
// |array|object (per ADR-0003): scalars round-trip through fmt-style
// formatting, structured values (decoded as map[string]any or []any
// by encoding/json) render as pretty-printed JSON so the user sees
// the full payload they are about to approve. Anything else
// (unexpected types from a malformed agent payload) falls through to
// Go's default %v.
func stringifyScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		// JSON numbers decode to float64 in Go. Format without a
		// trailing ".0" when the value is integral so the user sees
		// "10" rather than "10.000000" for max_results = 10.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case map[string]any, []any:
		// encoding/json never fails to marshal values of these shapes
		// when they were themselves decoded from JSON (the only path
		// agent payloads take into the runtime), so the error is
		// dropped — the resulting empty string would be more
		// misleading than helpful if this assumption ever broke.
		b, _ := json.MarshalIndent(x, "", "  ")
		return string(b)
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprintf("%v", v)
	}
}
