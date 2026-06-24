package runtime

import (
	"reflect"
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
