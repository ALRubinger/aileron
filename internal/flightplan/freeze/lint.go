package freeze

import (
	"fmt"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// validStepKinds is the closed set of step kinds the schema permits. The
// no-LLM guarantee is structural: only `llm-seam` reaches an LLM, and every
// other kind (action-call, transform, and the deterministic-subprocess tool
// step) is deterministic by construction. A step whose kind is absent or
// outside this set could smuggle a non-deterministic call past the seam, so
// the lint rejects it.
var validStepKinds = map[string]bool{
	"action-call": true,
	"transform":   true,
	"tool":        true,
	"llm-seam":    true,
}

// llmSeamKind is the single ADR-0027-permitted non-deterministic seam.
const llmSeamKind = "llm-seam"

// LintError reports a freeze-time lint failure and names the offending
// step so the author can fix it.
type LintError struct {
	StepID string
	Reason string
}

func (e *LintError) Error() string {
	if e.StepID != "" {
		return fmt.Sprintf("freeze lint: step %q: %s", e.StepID, e.Reason)
	}
	return "freeze lint: " + e.Reason
}

// Lint runs the no-LLM lint over a parsed manifest's step graph. It accepts
// a manifest whose only non-deterministic seam is an explicitly marked
// `kind: llm-seam` step, and rejects any step that could reach an LLM
// outside that seam.
//
// The structural rules, all keyed on the closed `kind` enum:
//
//   - every step must declare a `kind`;
//   - that kind must be one of the schema's closed set (action-call,
//     transform, tool, llm-seam); an unknown kind is a smuggling vector;
//   - a non-seam step must not carry an LLM marker (a key whose name marks
//     a model/prompt/llm call). Only `llm-seam` may.
//
// Instruction-only manifests and manifests with no steps lint clean: there
// is no composition that could reach an LLM.
func Lint(m *manifest.Manifest) error {
	if m == nil || m.InstructionOnly {
		return nil
	}
	// seenIDs enforces whole-graph unique step ids across ALL step kinds,
	// mirroring runtime/decode.go's launch-time duplicate check. Freeze runs
	// this before sealing stepTrust (steptrust.go) so a sealed reach can never
	// be keyed onto an id that an ambiguous non-tool step also declares:
	// steptrust.go only rejects duplicates among tool steps, leaving a
	// tool/non-tool id collision undetected without this whole-graph pass.
	seenIDs := map[string]bool{}
	for i, raw := range m.Aileron.Steps {
		step, ok := raw.(map[string]any)
		if !ok {
			return &LintError{
				StepID: stepIDOf(raw),
				Reason: fmt.Sprintf("step %d is not a mapping", i),
			}
		}
		id := scalarString(step["id"])
		if id != "" {
			if seenIDs[id] {
				return &LintError{
					StepID: id,
					Reason: "duplicate step id (every step id must be unique across the whole graph)",
				}
			}
			seenIDs[id] = true
		}
		kindVal, hasKind := step["kind"]
		if !hasKind {
			return &LintError{StepID: id, Reason: "missing kind (every step must declare a kind)"}
		}
		kind := scalarString(kindVal)
		if !validStepKinds[kind] {
			return &LintError{
				StepID: id,
				Reason: fmt.Sprintf("unknown step kind %q (only action-call, transform, tool, and the marked llm-seam may appear)", kind),
			}
		}
		if kind == llmSeamKind {
			// The explicitly marked seam is the one permitted LLM reach.
			continue
		}
		if marker := llmMarkerIn(step); marker != "" {
			return &LintError{
				StepID: id,
				Reason: fmt.Sprintf("kind %q carries an LLM marker %q outside the llm-seam (mark it kind: llm-seam or remove the LLM call)", kind, marker),
			}
		}
	}
	// Guard the embedded command-interpolation grammar: a `{{ inputs.<name> }}`
	// token in a tool step's sealed argv must name a declared, CONSTRAINED
	// input, or it is an injection surface. This runs after the whole-graph
	// kind/id pass so it only sees structurally valid steps.
	if err := lintCommandInterpolation(m); err != nil {
		return err
	}
	// Guard the embedded host-interpolation grammar (#1959): a
	// `{{ inputs.<name> }}` token in a tool step's sealed trustContract host
	// must name a declared, CONSTRAINED input, or it is an injection surface on
	// the step's egress. Same freeze-only constraint-presence rule as commands.
	if err := lintHostInterpolation(m); err != nil {
		return err
	}
	// Guard per-action (requires.actions[]) trustContract hosts (#1965): host
	// interpolation is tool-step-only, so an input token in a per-action host is
	// never substituted and would ride in inert as the literal `{{ ... }}`
	// string. Reject it at freeze rather than seal a syntactically valid but
	// inert host; per-action hosts must be literal.
	if err := lintActionHostLiterals(m); err != nil {
		return err
	}
	return nil
}

// llmMarkers are the top-level step keys that indicate a non-deterministic
// LLM call. A deterministic step (action-call, transform) that carries one
// of these is trying to reach an LLM outside the marked seam, which the
// lint rejects.
var llmMarkers = []string{"llm", "model", "prompt", "completion", "inference"}

// llmMarkerIn reports the first LLM marker key present on a step mapping, or
// "" when none is. The closed step schema (additionalProperties:false)
// already forbids these keys on a valid action-call/transform; the lint is
// the freeze-time backstop that names the offending marker rather than
// relying on schema validation alone.
func llmMarkerIn(step map[string]any) string {
	for _, marker := range llmMarkers {
		if _, ok := step[marker]; ok {
			return marker
		}
	}
	return ""
}

// stepIDOf best-effort extracts a step id from an untyped step for error
// messages, returning "" when the step has no usable id.
func stepIDOf(raw any) string {
	if m, ok := raw.(map[string]any); ok {
		return scalarString(m["id"])
	}
	return ""
}

// scalarString coerces a YAML-decoded scalar to a string. Non-string
// scalars (numbers, bools) format through fmt; nil yields "".
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}
