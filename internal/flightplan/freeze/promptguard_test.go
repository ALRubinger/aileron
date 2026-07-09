package freeze

import (
	"strings"
	"testing"
)

// seamStepWithPrompt builds a raw llm-seam step carrying the given prompt
// template and a single `text` output.
func seamStepWithPrompt(id, prompt string) map[string]any {
	return map[string]any{
		"id": id, "kind": llmSeamKind,
		"prompt":  prompt,
		"model":   "anthropic:claude-haiku-4-5",
		"outputs": []any{"text"},
	}
}

// transformStep builds a raw transform step producing the given outputs, used
// as the upstream a seam prompt references.
func transformStep(id string, outputs ...string) map[string]any {
	outs := make([]any, len(outputs))
	for i, o := range outputs {
		outs[i] = o
	}
	return map[string]any{"id": id, "kind": "transform", "outputs": outs}
}

// TestLint_PromptUndeclaredInputRejected proves a prompt referencing an input
// that is not declared at freeze is rejected, naming the seam and the token.
func TestLint_PromptUndeclaredInputRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("region")},
		[]any{seamStepWithPrompt("sum", "Report for {{ inputs.zone }}.")},
	)
	err := Lint(m)
	if err == nil {
		t.Fatal("a prompt referencing an undeclared input must be rejected")
	}
	if !strings.Contains(err.Error(), "undeclared input") ||
		!strings.Contains(err.Error(), "zone") || !strings.Contains(err.Error(), "sum") {
		t.Errorf("error should name the seam and the undeclared token, got: %v", err)
	}
}

// TestLint_PromptUnknownStepRejected proves a prompt referencing a step that
// does not exist is rejected, naming the missing step.
func TestLint_PromptUnknownStepRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		nil,
		[]any{seamStepWithPrompt("sum", "Summarize {{ steps.nope.out }}.")},
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "unknown step") ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("a prompt referencing an unknown step must be rejected naming it, got: %v", err)
	}
}

// TestLint_PromptUnknownOutputRejected proves a prompt referencing a KNOWN step
// but a non-existent output of it is rejected, naming the output and step.
func TestLint_PromptUnknownOutputRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		nil,
		[]any{
			transformStep("render", "csv"),
			seamStepWithPrompt("sum", "Summarize {{ steps.render.wrongout }}."),
		},
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "wrongout") ||
		!strings.Contains(err.Error(), "render") {
		t.Fatalf("a prompt referencing a non-existent output must be rejected naming it, got: %v", err)
	}
}

// TestLint_PromptMalformedTokenRejected proves a malformed brace shape in a
// prompt is rejected.
func TestLint_PromptMalformedTokenRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		nil,
		[]any{seamStepWithPrompt("sum", "Summarize {{ steps.render.csv }.")},
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a malformed prompt token must be rejected, got: %v", err)
	}
}

// TestLint_PromptBadBodyRejected proves a prompt token whose body is neither
// binding form is rejected.
func TestLint_PromptBadBodyRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		nil,
		[]any{seamStepWithPrompt("sum", "Summarize {{ outputs.x }}.")},
	)
	if err := Lint(m); err == nil {
		t.Fatal("a prompt token that is neither inputs.<name> nor steps.<id>.<output> must be rejected")
	}
}

// TestLint_PromptUnconstrainedInputAccepted is the load-bearing regression test
// that distinguishes the prompt guard from the command/host guards: a prompt
// referencing a declared-but-UNCONSTRAINED input lints CLEAN. A prompt is
// natural language handed to an LLM, not an injection surface, so the
// constrained-input clause was deliberately NOT added here.
func TestLint_PromptUnconstrainedInputAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{unconstrainedInput("window")},
		[]any{seamStepWithPrompt("sum", "Report the last {{ inputs.window }} days.")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a prompt referencing a declared unconstrained input must lint clean: %v", err)
	}
}

// TestLint_PromptConstrainedInputAccepted proves a prompt referencing a
// constrained input also lints clean (parity, no accidental rejection).
func TestLint_PromptConstrainedInputAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("region")},
		[]any{seamStepWithPrompt("sum", "Report for {{ inputs.region }}.")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a prompt referencing a constrained input must lint clean: %v", err)
	}
}

// TestLint_PromptValidStepOutputAccepted proves a prompt referencing a real
// steps.<id>.<output> pair produced by another step lints clean.
func TestLint_PromptValidStepOutputAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		nil,
		[]any{
			transformStep("render", "csv"),
			seamStepWithPrompt("sum", "Summarize {{ steps.render.csv }}."),
		},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a prompt referencing a valid step output must lint clean: %v", err)
	}
}

// TestLint_PromptTokenFreeAccepted proves a token-free prompt lints clean.
func TestLint_PromptTokenFreeAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		nil,
		[]any{seamStepWithPrompt("sum", "Summarize the run in one paragraph.")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a token-free prompt must lint clean: %v", err)
	}
}

// TestLint_PromptMultipleValidTokensAccepted proves a prompt with multiple
// mixed valid tokens (input + two step outputs) lints clean.
func TestLint_PromptMultipleValidTokensAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{unconstrainedInput("window")},
		[]any{
			transformStep("q", "series"),
			transformStep("render", "csv"),
			seamStepWithPrompt("sum",
				"For {{ inputs.window }} days summarize {{ steps.q.series }} rendered as {{ steps.render.csv }}."),
		},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a prompt with multiple valid tokens must lint clean: %v", err)
	}
}

// TestLint_SeamWithNoPromptAccepted proves an llm-seam carrying no prompt field
// lints clean (unchanged behavior from TestLint_AcceptsExplicitLLMSeam).
func TestLint_SeamWithNoPromptAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		nil,
		[]any{map[string]any{"id": "sum", "kind": llmSeamKind, "outputs": []any{"text"}}},
	)
	if err := Lint(m); err != nil {
		t.Errorf("an llm-seam with no prompt must lint clean: %v", err)
	}
}
