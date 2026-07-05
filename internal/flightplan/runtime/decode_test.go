package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// repoRoot walks up to the directory holding go.work so tests can read the
// committed worked example.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.work from %s", filepath.Dir(file))
		}
		dir = parent
	}
}

// exampleManifest parses the committed worked-example SKILL.md.
func exampleManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatalf("parse worked example: %v", err)
	}
	return m
}

// parseInline parses a SKILL.md literal through the full manifest.Parse path
// (schema validation included). Use it for cases the schema accepts.
func parseInline(t *testing.T, md string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(md))
	if err != nil {
		t.Fatalf("parse inline manifest: %v", err)
	}
	return m
}

// readActionReq is the minimal valid read-action requirement every fixture
// shares, so the runtime decoder always has the action a step can call.
func readActionReq() manifest.ActionRequirement {
	return manifest.ActionRequirement{
		Ref: "aileron:metrics.query_series",
		TrustContract: map[string]any{
			"credential":  map[string]any{"kind": "none"},
			"hosts":       []any{"api.example.com"},
			"effect":      "read",
			"idempotency": map[string]any{"safeToRetry": true},
			"audit":       map[string]any{"fields": []any{"result"}},
		},
	}
}

// rawManifest builds a *manifest.Manifest directly, bypassing schema
// validation, so the runtime decoder's OWN guards are exercised on inputs the
// schema would otherwise reject upstream. The runtime must refuse these
// independently (defense in depth: the manifest carries loosely-typed []any).
func rawManifest(inputs, outputs, steps []any) *manifest.Manifest {
	return &manifest.Manifest{
		Name: "t",
		Aileron: manifest.AileronBlock{
			SchemaVersion: "aileron.flightplan.v1",
			Requires:      manifest.Requires{Actions: []manifest.ActionRequirement{readActionReq()}},
			Inputs:        inputs,
			Outputs:       outputs,
			Steps:         steps,
		},
	}
}

func TestDecode_WorkedExample(t *testing.T) {
	p, err := Decode(exampleManifest(t))
	if err != nil {
		t.Fatalf("Decode worked example: %v", err)
	}
	if p.Name != "weekly-metrics-digest" {
		t.Errorf("name = %q", p.Name)
	}
	if len(p.Steps) != 4 {
		t.Fatalf("got %d steps, want 4", len(p.Steps))
	}
	if len(p.Order) != 4 {
		t.Fatalf("got order len %d, want 4", len(p.Order))
	}
	// query_metrics has no step deps, so it must come first in topo order.
	if p.Steps[p.Order[0]].ID != "query_metrics" {
		t.Errorf("first step = %q, want query_metrics", p.Steps[p.Order[0]].ID)
	}
	// file_issue depends on summarize, which depends on query_metrics +
	// render_csv, so it must come last.
	if p.Steps[p.Order[3]].ID != "file_issue" {
		t.Errorf("last step = %q, want file_issue", p.Steps[p.Order[3]].ID)
	}
	// Trust contract decoded: assert the action exists before inspecting its
	// effect, so a missing decode entry fails loudly rather than passing on a
	// zero-valued Effect.
	tracker, ok := p.Actions["aileron:tracker.create_issue"]
	if !ok {
		t.Fatal("tracker.create_issue must decode into the action set")
	}
	if tracker.TrustContract.Effect != EffectWrite {
		t.Error("tracker.create_issue effect must decode to write")
	}
	metrics, ok := p.Actions["aileron:metrics.query_series"]
	if !ok {
		t.Fatal("metrics.query_series must decode into the action set")
	}
	if metrics.TrustContract.Effect != EffectRead {
		t.Error("metrics.query_series effect must decode to read")
	}
}

func TestDecode_TopoOrderDeterministic(t *testing.T) {
	m := exampleManifest(t)
	var first []string
	for i := 0; i < 20; i++ {
		p, err := Decode(m)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		var order []string
		for _, idx := range p.Order {
			order = append(order, p.Steps[idx].ID)
		}
		if i == 0 {
			first = order
			continue
		}
		if strings.Join(order, ",") != strings.Join(first, ",") {
			t.Fatalf("topo order not deterministic: %v vs %v", order, first)
		}
	}
}

func TestDecode_InstructionOnlyRefused(t *testing.T) {
	md := "---\nname: x\ndescription: y\n---\n\n# X\n"
	m := parseInline(t, md)
	if _, err := Decode(m); err == nil {
		t.Fatal("instruction-only manifest must be refused")
	}
}

// step builds a raw step map of the shape the manifest carries (map[string]any).
func step(fields map[string]any) any { return fields }

func litInput(name, typ string, def any) any {
	res := map[string]any{"rule": "literal"}
	if def != nil {
		res["default"] = def
	}
	return map[string]any{"name": name, "type": typ, "resolution": res}
}

func TestDecode_UnknownStepKind(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{"id": "s1", "kind": "psychic", "outputs": []any{"x"}}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("unknown step kind must be refused")
	} else if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error = %v, want unknown kind", err)
	}
}

func TestDecode_TransformNameCarried(t *testing.T) {
	m := rawManifest(
		[]any{litInput("t", "string", "<b>{{.d}}</b>")},
		nil,
		[]any{step(map[string]any{
			"id": "render", "kind": "transform", "transform": "html-render",
			"bindings": map[string]any{"template": "inputs.t"},
			"outputs":  []any{"report"},
		})},
	)
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if p.Steps[0].Transform != "html-render" {
		t.Errorf("transform name = %q, want html-render", p.Steps[0].Transform)
	}
}

func TestDecode_TransformNameOnActionCallRefused(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "call", "kind": "action-call",
			"actionRef": "aileron:metrics.query_series",
			"transform": "html-render",
			"outputs":   []any{"x"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("an action-call naming a transform must be refused")
	} else if !strings.Contains(err.Error(), "transform") {
		t.Errorf("error = %v, want a transform-name rejection", err)
	}
}

func TestDecode_TransformNameOnLLMSeamRefused(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "seam", "kind": "llm-seam",
			"transform": "html-render",
			"outputs":   []any{"x"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("an llm-seam naming a transform must be refused")
	}
}

func TestDecode_MalformedBinding(t *testing.T) {
	m := rawManifest(
		[]any{litInput("w", "number", 7)},
		nil,
		[]any{step(map[string]any{
			"id": "s1", "kind": "transform",
			"bindings": map[string]any{"v": "not-a-binding"},
			"outputs":  []any{"x"},
		})},
	)
	if _, err := Decode(m); err == nil {
		t.Fatal("binding outside the closed grammar must be refused")
	}
}

func TestDecode_BindingToUndeclaredInput(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "s1", "kind": "transform",
			"bindings": map[string]any{"v": "inputs.missing"},
			"outputs":  []any{"x"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("binding to undeclared input must be refused")
	}
}

func TestDecode_BindingToAbsentStep(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "s1", "kind": "transform",
			"bindings": map[string]any{"v": "steps.ghost.out"},
			"outputs":  []any{"x"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("binding to an absent step must be refused")
	}
}

func TestDecode_BindingToAbsentStepOutput(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{"id": "a", "kind": "transform", "outputs": []any{"x"}}),
		step(map[string]any{
			"id": "b", "kind": "transform",
			"bindings": map[string]any{"v": "steps.a.nope"},
			"outputs":  []any{"y"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("binding to a non-existent output of a real step must be refused")
	}
}

func TestDecode_CyclicStepsRefused(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "a", "kind": "transform",
			"bindings": map[string]any{"v": "steps.b.y"},
			"outputs":  []any{"x"},
		}),
		step(map[string]any{
			"id": "b", "kind": "transform",
			"bindings": map[string]any{"v": "steps.a.x"},
			"outputs":  []any{"y"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("a steps.* cycle must be refused")
	} else if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want cycle", err)
	}
}

// A source input referencing the same action a step calls is NOT a graph edge
// and must NOT trip the cycle check (#1523 boundary).
func TestDecode_SourceInputIsNotAStepEdge(t *testing.T) {
	m := rawManifest(
		[]any{map[string]any{
			"name": "live", "type": "array",
			"resolution": map[string]any{
				"rule": "source",
				"source": map[string]any{
					"actionRef": "aileron:metrics.query_series",
					"select":    "series[].name",
				},
			},
		}},
		nil,
		[]any{step(map[string]any{
			"id": "query", "kind": "action-call",
			"actionRef": "aileron:metrics.query_series",
			"args":      map[string]any{"metric_set": "inputs.live"},
			"outputs":   []any{"series"},
		})},
	)
	if _, err := Decode(m); err != nil {
		t.Fatalf("a source input on the same action a step calls must not trip the cycle check: %v", err)
	}
}

// constrainedInput builds a raw literal input map carrying a constraint block,
// the shape the manifest carries (map[string]any), so the runtime decoder's own
// constraint guards run on inputs the schema would otherwise reject upstream.
func constrainedInput(name string, constraint map[string]any) any {
	return map[string]any{
		"name": name, "type": "string",
		"resolution": map[string]any{"rule": "literal"},
		"constraint": constraint,
	}
}

// oneStep is a trivial valid step so Decode reaches the input decode and, on
// valid inputs, completes with a non-empty step graph.
func oneStep() []any {
	return []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"x"}})}
}

func TestDecode_ConstraintEnumPopulated(t *testing.T) {
	m := rawManifest(
		[]any{constrainedInput("env", map[string]any{"enum": []any{"prod", "staging"}})},
		nil, oneStep(),
	)
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c := p.Inputs[0].Constraint
	if c == nil {
		t.Fatal("constraint must be populated")
	}
	if c.Pattern != nil {
		t.Error("an enum constraint must not compile a pattern")
	}
	if len(c.Enum) != 2 || c.Enum[0] != "prod" || c.Enum[1] != "staging" {
		t.Errorf("enum = %v", c.Enum)
	}
}

func TestDecode_ConstraintPatternCompiled(t *testing.T) {
	m := rawManifest(
		[]any{constrainedInput("region", map[string]any{"pattern": "^us-[a-z]+-[0-9]$"})},
		nil, oneStep(),
	)
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c := p.Inputs[0].Constraint
	if c == nil || c.Pattern == nil {
		t.Fatal("pattern constraint must compile a regexp")
	}
	if len(c.Enum) != 0 {
		t.Error("a pattern constraint must carry no enum")
	}
	if !c.Pattern.MatchString("us-east-1") {
		t.Error("compiled pattern must match a valid region")
	}
}

// A YAML block scalar can append a trailing newline to a pattern. The decoder
// must normalize it so the enforced regexp is the author's anchored pattern,
// not one that also requires a literal trailing newline.
func TestDecode_ConstraintPatternWhitespaceNormalized(t *testing.T) {
	m := rawManifest(
		[]any{constrainedInput("region", map[string]any{"pattern": "^us-east-1$\n"})},
		nil, oneStep(),
	)
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c := p.Inputs[0].Constraint
	if c == nil || c.Pattern == nil {
		t.Fatal("pattern constraint must compile a regexp")
	}
	if !c.Pattern.MatchString("us-east-1") {
		t.Error("a trailing newline in the pattern must be normalized away, so the anchored value still matches")
	}
	if c.Pattern.String() != "^us-east-1$" {
		t.Errorf("stored pattern = %q, want the trimmed ^us-east-1$", c.Pattern.String())
	}
}

func TestDecode_ConstraintUncompilablePatternRefused(t *testing.T) {
	// A pattern the schema cannot catch (ECMA-valid shape, RE2-invalid): an
	// unclosed group. This is the fail-closed case only decode enforces.
	m := rawManifest(
		[]any{constrainedInput("bad", map[string]any{"pattern": "([a-z"})},
		nil, oneStep(),
	)
	if _, err := Decode(m); err == nil {
		t.Fatal("an uncompilable RE2 pattern must be refused at decode")
	} else if !strings.Contains(err.Error(), "compile") {
		t.Errorf("error = %v, want a compile rejection", err)
	}
}

func TestDecode_ConstraintBothEnumAndPatternRefused(t *testing.T) {
	m := rawManifest(
		[]any{constrainedInput("env", map[string]any{"enum": []any{"prod"}, "pattern": "^prod$"})},
		nil, oneStep(),
	)
	if _, err := Decode(m); err == nil {
		t.Fatal("a constraint declaring both enum and pattern must be refused")
	} else if !strings.Contains(err.Error(), "both") {
		t.Errorf("error = %v, want a both-present rejection", err)
	}
}

func TestDecode_ConstraintEmptyEnumRefused(t *testing.T) {
	m := rawManifest(
		[]any{constrainedInput("env", map[string]any{"enum": []any{}})},
		nil, oneStep(),
	)
	if _, err := Decode(m); err == nil {
		t.Fatal("a constraint with neither a non-empty enum nor a pattern must be refused")
	}
}

func TestDecode_ConstraintEmptyEnumEntryRefused(t *testing.T) {
	m := rawManifest(
		[]any{constrainedInput("env", map[string]any{"enum": []any{"prod", ""}})},
		nil, oneStep(),
	)
	if _, err := Decode(m); err == nil {
		t.Fatal("a constraint enum carrying an empty value must be refused")
	}
}

func TestDecode_DuplicateStepID(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{"id": "dup", "kind": "transform", "outputs": []any{"x"}}),
		step(map[string]any{"id": "dup", "kind": "transform", "outputs": []any{"y"}}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("duplicate step id must be refused")
	}
}

func TestDecode_MaterializesUndeclaredOutput(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "s1", "kind": "transform", "outputs": []any{"x"},
			"materializesOutput": "ghost.csv",
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("materializing an undeclared output must be refused")
	}
}

func TestDecode_ActionCallToUndeclaredAction(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "s1", "kind": "action-call",
			"actionRef": "aileron:other.thing",
			"outputs":   []any{"x"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("action-call to an action not in requires.actions must be refused")
	}
}

// TestDecode_ToolOnlyPlanNoActions is the decode-side half of the #1932
// guarantee: a plan that declares no requires.actions and runs only a tool
// step decodes cleanly. The counterpart guardrail
// (TestDecode_ActionCallToUndeclaredAction) proves a plan that DOES call an
// action still needs the ref declared, so relaxing the schema to make actions
// optional does not weaken the declared-ref check.
func TestDecode_ToolOnlyPlanNoActions(t *testing.T) {
	m := &manifest.Manifest{
		Name: "tool-only",
		Aileron: manifest.AileronBlock{
			SchemaVersion: "aileron.flightplan.v1",
			// Requires left zero: no declared actions, as a tool-only plan.
			Steps: []any{
				step(map[string]any{
					"id": "render", "kind": "tool",
					"command": []any{"aws", "s3", "ls"},
					"outputs": []any{"listing"},
				}),
			},
		},
	}
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("a tool-only plan with no declared actions must decode, got: %v", err)
	}
	if len(p.Actions) != 0 {
		t.Errorf("a tool-only plan decodes no actions, got: %v", p.Actions)
	}
	if len(p.Steps) != 1 || p.Steps[0].Kind != KindTool {
		t.Fatalf("expected one tool step, got: %+v", p.Steps)
	}
}

func TestDecodeError_Type(t *testing.T) {
	_, err := Decode(parseInline(t, "---\nname: x\ndescription: y\n---\n\n# X\n"))
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not a *DecodeError", err)
	}
}

func TestDecode_ActionCallWithBindingsRefused(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "s1", "kind": "action-call", "actionRef": "aileron:metrics.query_series",
			"bindings": map[string]any{"v": "inputs.x"},
			"outputs":  []any{"o"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("an action-call carrying bindings must be refused")
	}
}

func TestDecode_TransformWithArgsRefused(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "s1", "kind": "transform",
			"args":    map[string]any{"v": "inputs.x"},
			"outputs": []any{"o"},
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("a transform carrying args must be refused")
	}
}

func TestDecode_UnknownStepFieldRefused(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{
			"id": "s1", "kind": "transform", "outputs": []any{"o"},
			"bogusTypo": true,
		}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("a step with an unknown field must be refused (strict decode)")
	}
}

func TestDecode_DuplicateOutputRefused(t *testing.T) {
	out := func() any {
		return map[string]any{"name": "dup.csv", "mimeType": "text/csv", "encoding": "utf-8",
			"publish": map[string]any{"target": "none"}}
	}
	m := rawManifest(nil, []any{out(), out()},
		[]any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})})
	if _, err := Decode(m); err == nil {
		t.Fatal("a duplicate output name must be refused")
	}
}

func TestDecode_UnknownEncodingRefused(t *testing.T) {
	m := rawManifest(nil, []any{map[string]any{
		"name": "o", "mimeType": "text/csv", "encoding": "rot13",
		"publish": map[string]any{"target": "none"},
	}}, []any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})})
	if _, err := Decode(m); err == nil {
		t.Fatal("an unknown output encoding must be refused")
	}
}

func TestDecode_FilePublishNeedsPath(t *testing.T) {
	m := rawManifest(nil, []any{map[string]any{
		"name": "o", "mimeType": "text/csv", "encoding": "utf-8",
		"publish": map[string]any{"target": "file"},
	}}, []any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})})
	if _, err := Decode(m); err == nil {
		t.Fatal("a file publish target without a path must be refused")
	}
}

func TestDecode_UnknownInputTypeRefused(t *testing.T) {
	m := rawManifest([]any{map[string]any{
		"name": "i", "type": "quaternion", "resolution": map[string]any{"rule": "literal"},
	}}, nil, []any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})})
	if _, err := Decode(m); err == nil {
		t.Fatal("an unknown input type must be refused")
	}
}

func TestDecode_UnknownResolutionRuleRefused(t *testing.T) {
	m := rawManifest([]any{map[string]any{
		"name": "i", "type": "string", "resolution": map[string]any{"rule": "telepathy"},
	}}, nil, []any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})})
	if _, err := Decode(m); err == nil {
		t.Fatal("an unknown resolution rule must be refused")
	}
}

func TestDecode_DynamicValueValidated(t *testing.T) {
	m := rawManifest([]any{map[string]any{
		"name": "i", "type": "timestamp",
		"resolution": map[string]any{"rule": "dynamic", "value": "yesterday"},
	}}, nil, []any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})})
	if _, err := Decode(m); err == nil {
		t.Fatal("a dynamic value other than now/today must be refused")
	}
}

func TestDecode_SourceWithoutActionRefRefused(t *testing.T) {
	m := rawManifest([]any{map[string]any{
		"name": "i", "type": "array",
		"resolution": map[string]any{"rule": "source", "source": map[string]any{"select": "x"}},
	}}, nil, []any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})})
	if _, err := Decode(m); err == nil {
		t.Fatal("a source resolution without an actionRef must be refused")
	}
}

func TestDecode_UnknownEffectRefused(t *testing.T) {
	ar := readActionReq()
	ar.TrustContract["effect"] = "teleport"
	m := &manifest.Manifest{Name: "t", Aileron: manifest.AileronBlock{
		Requires: manifest.Requires{Actions: []manifest.ActionRequirement{ar}},
		Steps:    []any{step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x"}})},
	}}
	if _, err := Decode(m); err == nil {
		t.Fatal("an unknown trust-contract effect must be refused")
	}
}

func TestDecode_DuplicateStepOutputRefused(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{"id": "s", "kind": "transform", "outputs": []any{"x", "x"}}),
	})
	if _, err := Decode(m); err == nil {
		t.Fatal("a step with duplicate output names must be refused")
	}
}
