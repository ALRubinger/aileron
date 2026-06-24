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
	// Trust contract decoded: tracker.create_issue is a write.
	if p.Actions["aileron:tracker.create_issue"].TrustContract.Effect != EffectWrite {
		t.Error("tracker.create_issue effect must decode to write")
	}
	if p.Actions["aileron:metrics.query_series"].TrustContract.Effect != EffectRead {
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
