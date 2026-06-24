package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestRunSeam_NilProviderErrorsByDefault(t *testing.T) {
	step := Step{ID: "summarize", Kind: KindLLMSeam, Outputs: []string{"issue_body"}}
	_, err := runSeam(context.Background(), nil, step, map[string]any{"series": "x"})
	if err == nil {
		t.Fatal("an llm-seam step with no provider must error (v1 reaches no LLM by default)")
	}
	if !strings.Contains(err.Error(), "no seam provider") {
		t.Errorf("error = %v, want 'no seam provider'", err)
	}
}

// fakeSeam is a test LLM seam. It is the ONLY way a test reaches the seam,
// proving the seam is opt-in.
type fakeSeam struct {
	out map[string]any
}

func (f fakeSeam) Run(_ context.Context, req SeamRequest) (map[string]any, error) {
	if f.out != nil {
		return f.out, nil
	}
	out := map[string]any{}
	for _, name := range req.Outputs {
		out[name] = "seam-" + name
	}
	return out, nil
}

func TestRunSeam_ProducesDeclaredOutputs(t *testing.T) {
	step := Step{ID: "s", Kind: KindLLMSeam, Outputs: []string{"issue_body"}}
	out, err := runSeam(context.Background(), fakeSeam{}, step, map[string]any{"series": "x"})
	if err != nil {
		t.Fatalf("runSeam: %v", err)
	}
	if out["issue_body"] != "seam-issue_body" {
		t.Errorf("seam output = %v", out["issue_body"])
	}
}

func TestRunSeam_MissingOutputErrors(t *testing.T) {
	step := Step{ID: "s", Kind: KindLLMSeam, Outputs: []string{"a", "b"}}
	seam := fakeSeam{out: map[string]any{"a": 1}} // missing "b"
	if _, err := runSeam(context.Background(), seam, step, nil); err == nil {
		t.Fatal("a seam that omits a declared output must error")
	}
}
