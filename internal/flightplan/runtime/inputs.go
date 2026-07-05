package runtime

import (
	"context"
	"fmt"
	"time"
)

// ResolvedInputs is the frozen, read-only set of input values produced once at
// the launch boundary (#1523). Phase B (the DAG walk) consumes it without
// re-resolving: a dynamic input is read from the clock exactly once, so two
// steps reading the same input see one value.
type ResolvedInputs struct {
	// Values maps input name → resolved value.
	Values map[string]any
	// SourceBindings records, per source input, the resolved binding the read
	// was recorded by (action ref + select), never the dataset inline. This
	// is what the audit references (ADR-0027 audit boundary).
	SourceBindings map[string]SourceBinding
}

// SourceBinding is the audit-safe record of a source input resolution: the
// action ref and selector that produced it. The resolved dataset is never
// stored inline here.
type SourceBinding struct {
	ActionRef string
	Select    string
}

// LaunchArgs are the literal input overrides supplied at launch (the
// `--input name=value` CLI flags). A literal input takes its launch override,
// then its declared default; a missing required literal (no override, no
// default) is an error.
type LaunchArgs map[string]any

// resolveInputs runs Phase A: it resolves every declared input ONCE into a
// concrete ResolvedInputs set. literal → launch arg or default; dynamic → a
// single read of the injected clock (now/today); source → one dispatch through
// the enforced action boundary, recorded by resolved binding.
//
// The clock is read at most once and the value reused for every dynamic input,
// so the launch-boundary straddle (#1523) cannot give two steps different
// clock values.
func resolveInputs(ctx context.Context, p *Plan, args LaunchArgs, clk Clock, e *enforcer) (ResolvedInputs, error) {
	if args == nil {
		args = LaunchArgs{}
	}
	ri := ResolvedInputs{
		Values:         map[string]any{},
		SourceBindings: map[string]SourceBinding{},
	}

	// Read the clock once at the boundary; reuse for every dynamic input.
	launchTime := clk.Now()

	for _, in := range p.Inputs {
		switch in.Resolution.Rule {
		case ResolutionLiteral:
			val, err := resolveLiteral(in, args)
			if err != nil {
				return ResolvedInputs{}, err
			}
			// Deep-copy so the frozen resolved set never aliases the caller's
			// launch args or the declared default: a downstream step that
			// mutates an input value can never reach back into the source.
			ri.Values[in.Name] = deepCopyValue(val)
		case ResolutionDynamic:
			ri.Values[in.Name] = resolveDynamic(in, launchTime)
		case ResolutionSource:
			val, sb, err := resolveSource(ctx, p, in, e)
			if err != nil {
				return ResolvedInputs{}, err
			}
			// Deep-copy so the resolved set does not alias the dispatch result.
			ri.Values[in.Name] = deepCopyValue(val)
			ri.SourceBindings[in.Name] = sb
		default:
			return ResolvedInputs{}, fmt.Errorf("input %q: unhandled resolution rule %q", in.Name, in.Resolution.Rule)
		}
	}

	// Enforce declared constraints against the resolved values as a final pass,
	// once every input has a value. This is the single launch/resolution
	// boundary where a resolved value exists: an out-of-constraint value fails
	// the launch closed here, with no permissive fallback. An input with no
	// declared constraint is unchecked (today's behavior).
	for _, in := range p.Inputs {
		if in.Constraint == nil {
			continue
		}
		if err := enforceConstraint(in.Name, ri.Values[in.Name], in.Constraint); err != nil {
			return ResolvedInputs{}, err
		}
	}
	return ri, nil
}

// maxResolvedValueInError caps how much of a resolved value's string form is
// embedded in an enforcement error. A resolved source or literal value can be
// arbitrarily large (a fetched dataset, a long string), so the error message is
// bounded here rather than echoing the whole value back to the operator.
const maxResolvedValueInError = 256

// capResolvedValue truncates s to maxResolvedValueInError characters, appending
// a marker when it was longer so the truncation is visible. The comparison
// against the constraint still uses the full string form; only the message is
// capped.
func capResolvedValue(s string) string {
	if len(s) <= maxResolvedValueInError {
		return s
	}
	return s[:maxResolvedValueInError] + "...(truncated)"
}

// enforceConstraint checks a resolved value against its declared constraint.
// The comparison is over the value's string form (fmt.Sprintf("%v", v)), so a
// number or timestamp input stays checkable: enum requires equality with one
// allowed string, pattern requires the compiled regexp to match. A violation
// names the input, the (capped) value, and the constraint so the failure is
// actionable without echoing an unbounded resolved value back to the operator.
func enforceConstraint(name string, v any, c *Constraint) error {
	s := fmt.Sprintf("%v", v)
	capped := capResolvedValue(s)
	if c.Pattern != nil {
		if !c.Pattern.MatchString(s) {
			return fmt.Errorf("input %q resolved to %q, which does not match the required pattern %q", name, capped, c.Pattern.String())
		}
		return nil
	}
	for _, allowed := range c.Enum {
		if s == allowed {
			return nil
		}
	}
	return fmt.Errorf("input %q resolved to %q, which is not one of the allowed values %v", name, capped, c.Enum)
}

// resolveLiteral resolves a literal input: the launch override wins, then the
// declared default. A literal with neither is a missing required input.
func resolveLiteral(in Input, args LaunchArgs) (any, error) {
	if v, ok := args[in.Name]; ok {
		return v, nil
	}
	if in.Resolution.HasDefault {
		return in.Resolution.Default, nil
	}
	return nil, fmt.Errorf("input %q is required: pass it at launch (it declares no default)", in.Name)
}

// resolveDynamic reads the single launch-time clock value. `now` resolves to
// the RFC3339 launch timestamp; `today` resolves to the launch date.
func resolveDynamic(in Input, launchTime time.Time) any {
	switch in.Resolution.DynamicValue {
	case "now":
		return launchTime.Format(time.RFC3339)
	case "today":
		return launchTime.Format("2006-01-02")
	default:
		// Decode validated the value; this branch is unreachable.
		return launchTime.Format(time.RFC3339)
	}
}

// resolveSource resolves a source input by ONE dispatch through the enforced
// action boundary, applying the select and recording the resolved binding.
// The source read is NOT a step and not a graph edge (#1523): it is resolved
// in Phase A, before the DAG walk.
func resolveSource(ctx context.Context, p *Plan, in Input, e *enforcer) (any, SourceBinding, error) {
	ref := in.Resolution.SourceActionRef
	action, ok := p.Actions[ref]
	if !ok {
		return nil, SourceBinding{}, fmt.Errorf("input %q: source action %q is not declared in requires.actions", in.Name, ref)
	}
	out, err := e.dispatch(ctx, "input:"+in.Name, action, map[string]any{}, 1)
	if err != nil {
		return nil, SourceBinding{}, fmt.Errorf("input %q: resolve from source %q: %w", in.Name, ref, err)
	}
	sb := SourceBinding{ActionRef: ref, Select: in.Resolution.SourceSelect}
	val := applySelect(out.Result, in.Resolution.SourceSelect)
	return val, sb, nil
}

// applySelect applies the source selector to a dispatch result. v1 supports a
// dotted path with the `[]` array-element wildcard (the same grammar redaction
// uses), returning the selected value. An empty selector returns the whole
// result. A selector that matches nothing returns nil.
func applySelect(result map[string]any, sel string) any {
	if sel == "" {
		return result
	}
	return selectPath(any(result), parsePath(sel))
}

// selectPath walks a select path, collecting array-wildcard results into a
// slice so `series[].name` yields every series name.
func selectPath(node any, segs []pathSeg) any {
	if len(segs) == 0 {
		return node
	}
	seg := segs[0]
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	child, present := m[seg.key]
	if !present {
		return nil
	}
	if seg.wildcard {
		arr, ok := child.([]any)
		if !ok {
			return nil
		}
		rest := segs[1:]
		out := make([]any, 0, len(arr))
		for _, el := range arr {
			out = append(out, selectPath(el, rest))
		}
		return out
	}
	return selectPath(child, segs[1:])
}
