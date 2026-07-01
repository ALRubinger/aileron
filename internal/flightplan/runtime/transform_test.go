package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func TestTransform_Deterministic(t *testing.T) {
	reg := NewTransformRegistry()
	step := Step{ID: "s", Kind: KindTransform, Outputs: []string{"out"}}
	bind := map[string]any{"series": []any{1, 2, 3}}
	a, err := reg.run(step, bind)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := reg.run(step, bind)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("transform not deterministic: %v vs %v", a, b)
	}
}

func TestTransform_SingleOutputSingleBindingForwards(t *testing.T) {
	reg := NewTransformRegistry()
	step := Step{ID: "s", Kind: KindTransform, Outputs: []string{"csv"}}
	out, err := reg.run(step, map[string]any{"series": "data"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out["csv"] != "data" {
		t.Errorf("single-output single-binding must forward the value, got %v", out["csv"])
	}
}

func TestTransform_NoOutputsErrors(t *testing.T) {
	if _, err := identityTransform(map[string]any{}, nil); err == nil {
		t.Fatal("a transform with no declared outputs must error")
	}
}

func TestTransform_RegisterCustom(t *testing.T) {
	reg := NewTransformRegistry()
	reg.Register("upper", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: "X"}, nil
	})
	if _, ok := reg.byName["upper"]; !ok {
		t.Error("Register must add the transform")
	}
}

func TestTransform_NamedLookup(t *testing.T) {
	reg := NewTransformRegistry()
	reg.Register("upper", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: "X"}, nil
	})
	// A step naming a transform selects it from the registry rather than the
	// identity default.
	step := Step{ID: "s", Kind: KindTransform, Transform: "upper", Outputs: []string{"out"}}
	out, err := reg.run(step, map[string]any{"in": "y"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out["out"] != "X" {
		t.Errorf("named transform not selected, got %v", out["out"])
	}
}

func TestTransform_UnnamedFallsBackToIdentity(t *testing.T) {
	reg := NewTransformRegistry()
	// No Transform name: the identity passthrough forwards a single binding.
	step := Step{ID: "s", Kind: KindTransform, Outputs: []string{"csv"}}
	out, err := reg.run(step, map[string]any{"series": "data"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out["csv"] != "data" {
		t.Errorf("unnamed step must fall back to identity, got %v", out["csv"])
	}
}

func TestTransform_UnknownNameErrors(t *testing.T) {
	reg := NewTransformRegistry()
	step := Step{ID: "s", Kind: KindTransform, Transform: "no-such-transform", Outputs: []string{"out"}}
	if _, err := reg.run(step, map[string]any{}); err == nil {
		t.Fatal("an unregistered transform name must be a hard error, not a silent identity fallback")
	}
}

func TestHTMLRender_EmitsSealedFileMapEntry(t *testing.T) {
	reg := NewTransformRegistry()
	step := Step{ID: "s", Kind: KindTransform, Transform: "html-render", Outputs: []string{"report"}}
	bind := map[string]any{
		"template": "<h1>{{ .report.title }}</h1><p>{{ .report.count }}</p>",
		"report":   map[string]any{"title": "Weekly", "count": float64(3)},
	}
	out, err := reg.run(step, bind)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	entry, ok := out["report"].(map[string]any)
	if !ok {
		t.Fatalf("html-render must emit a file-map entry map, got %T", out["report"])
	}
	if entry["mimeType"] != "text/html" {
		t.Errorf("mimeType = %v, want text/html", entry["mimeType"])
	}
	if entry["encoding"] != "utf-8" {
		t.Errorf("encoding = %v, want utf-8", entry["encoding"])
	}
	content, _ := entry["content"].(string)
	if content != "<h1>Weekly</h1><p>3</p>" {
		t.Errorf("content = %q", content)
	}
}

func TestHTMLRender_DeterministicAndPure(t *testing.T) {
	reg := NewTransformRegistry()
	step := Step{ID: "s", Kind: KindTransform, Transform: "html-render", Outputs: []string{"report"}}
	// Multiple named datasets, including a map whose key order is non-obvious,
	// to exercise html/template's sorted-key range determinism.
	bind := map[string]any{
		"template": "{{ range $k, $v := .rows }}{{ $k }}={{ $v }};{{ end }}",
		"rows":     map[string]any{"zebra": "1", "alpha": "2", "mango": "3"},
	}
	first, err := reg.run(step, bind)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	firstContent := first["report"].(map[string]any)["content"].(string)
	// Sorted keys: alpha, mango, zebra.
	if firstContent != "alpha=2;mango=3;zebra=1;" {
		t.Errorf("html/template map range must be sorted-key deterministic, got %q", firstContent)
	}
	for i := 0; i < 25; i++ {
		got, err := reg.run(step, bind)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if got["report"].(map[string]any)["content"].(string) != firstContent {
			t.Fatalf("html-render not deterministic on run %d", i)
		}
	}
}

func TestHTMLRender_AutoEscapesInjectedData(t *testing.T) {
	reg := NewTransformRegistry()
	step := Step{ID: "s", Kind: KindTransform, Transform: "html-render", Outputs: []string{"report"}}
	bind := map[string]any{
		"template": "<div>{{ .payload }}</div>",
		"payload":  "<script>alert('x')</script>",
	}
	out, err := reg.run(step, bind)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	content := out["report"].(map[string]any)["content"].(string)
	if strings.Contains(content, "<script>") {
		t.Errorf("html/template must auto-escape injected data, got %q", content)
	}
}

func TestHTMLRender_MissingTemplateBindingErrors(t *testing.T) {
	reg := NewTransformRegistry()
	step := Step{ID: "s", Kind: KindTransform, Transform: "html-render", Outputs: []string{"report"}}
	if _, err := reg.run(step, map[string]any{"data": "x"}); err == nil {
		t.Fatal("html-render without a template binding must error")
	}
}

func TestHTMLRender_NonStringTemplateErrors(t *testing.T) {
	if _, err := htmlRenderTransform(map[string]any{"template": 42}, []string{"report"}); err == nil {
		t.Fatal("a non-string template binding must error")
	}
}

func TestHTMLRender_ParseErrorReported(t *testing.T) {
	if _, err := htmlRenderTransform(map[string]any{"template": "{{ .broken"}, []string{"report"}); err == nil {
		t.Fatal("a malformed template must surface a parse error")
	}
}

func TestHTMLRender_MultipleOutputsErrors(t *testing.T) {
	if _, err := htmlRenderTransform(map[string]any{"template": "x"}, []string{"a", "b"}); err == nil {
		t.Fatal("html-render must produce exactly one file-map entry")
	}
}

func TestHTMLRender_AdvisoryPathBindingCarried(t *testing.T) {
	out, err := htmlRenderTransform(map[string]any{
		"template": "<p>{{ .d }}</p>", "path": "report.html", "d": "hi",
	}, []string{"report"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	entry := out["report"].(map[string]any)
	if entry["path"] != "report.html" {
		t.Errorf("advisory path must be carried into the file-map entry, got %v", entry["path"])
	}
	if entry["content"].(string) != "<p>hi</p>" {
		t.Errorf("path/template bindings must be excluded from the data context, got %q", entry["content"])
	}
}
