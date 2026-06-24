package freeze

import (
	"errors"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

func TestLint_AcceptsWorkedExampleWithMarkedSeam(t *testing.T) {
	m, err := manifest.Parse(exampleSkillMD(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Lint(m); err != nil {
		t.Errorf("worked example with a marked llm-seam must lint clean: %v", err)
	}
}

func TestLint_InstructionOnlyClean(t *testing.T) {
	m, err := manifest.Parse([]byte(instructionOnlyMD))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Lint(m); err != nil {
		t.Errorf("instruction-only skill must lint clean: %v", err)
	}
}

// A manifest is constructed directly (bypassing manifest.Parse schema
// validation) so the lint's own structural rules are exercised: the lint is
// the freeze-time backstop, not a re-run of schema validation.
func manifestWithSteps(steps []any) *manifest.Manifest {
	return &manifest.Manifest{
		Name: "linttest",
		Aileron: manifest.AileronBlock{
			Steps: steps,
		},
	}
}

func TestLint_RejectsUnmarkedLLMStep(t *testing.T) {
	// An action-call step that carries an LLM marker outside the seam.
	m := manifestWithSteps([]any{
		map[string]any{"id": "sneaky", "kind": "action-call", "actionRef": "aileron:x.y", "prompt": "summarize this"},
	})
	err := Lint(m)
	if err == nil {
		t.Fatal("an unmarked LLM call must fail lint")
	}
	var le *LintError
	if !errors.As(err, &le) {
		t.Fatalf("want *LintError, got %T", err)
	}
	if le.StepID != "sneaky" {
		t.Errorf("error must name the offending step, got %q", le.StepID)
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("error should name the LLM marker, got: %v", err)
	}
}

func TestLint_AcceptsExplicitLLMSeam(t *testing.T) {
	m := manifestWithSteps([]any{
		map[string]any{"id": "summarize", "kind": "llm-seam", "outputs": []any{"text"}},
	})
	if err := Lint(m); err != nil {
		t.Errorf("an explicitly marked llm-seam must lint clean: %v", err)
	}
}

func TestLint_RejectsUnknownKind(t *testing.T) {
	m := manifestWithSteps([]any{
		map[string]any{"id": "weird", "kind": "magic-call"},
	})
	err := Lint(m)
	if err == nil {
		t.Fatal("an unknown step kind must fail lint")
	}
	if !strings.Contains(err.Error(), "weird") || !strings.Contains(err.Error(), "magic-call") {
		t.Errorf("error should name the step and the bad kind, got: %v", err)
	}
}

func TestLint_RejectsMissingKind(t *testing.T) {
	m := manifestWithSteps([]any{
		map[string]any{"id": "nokind"},
	})
	err := Lint(m)
	if err == nil {
		t.Fatal("a step with no kind must fail lint")
	}
	if !strings.Contains(err.Error(), "nokind") {
		t.Errorf("error should name the step, got: %v", err)
	}
}

func TestLint_RejectsNonMappingStep(t *testing.T) {
	m := manifestWithSteps([]any{"not-a-mapping"})
	if err := Lint(m); err == nil {
		t.Error("a non-mapping step must fail lint")
	}
}

func TestLint_AcceptsCleanTransform(t *testing.T) {
	m := manifestWithSteps([]any{
		map[string]any{"id": "render", "kind": "transform", "outputs": []any{"csv"}},
		map[string]any{"id": "call", "kind": "action-call", "actionRef": "aileron:x.y"},
	})
	if err := Lint(m); err != nil {
		t.Errorf("clean transform + action-call must lint clean: %v", err)
	}
}

func TestLint_RejectsModelMarkerOnTransform(t *testing.T) {
	m := manifestWithSteps([]any{
		map[string]any{"id": "t", "kind": "transform", "outputs": []any{"x"}, "model": "gpt-4"},
	})
	if err := Lint(m); err == nil {
		t.Error("a transform carrying a model marker must fail lint")
	}
}
