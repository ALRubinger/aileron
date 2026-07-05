package freeze

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// constrainedInput builds a raw input map declaring the given name with a
// single-value enum constraint, the shape freeze reads to decide an input is
// safe to interpolate into a sealed command.
func constrainedInput(name string) map[string]any {
	return map[string]any{
		"name": name, "type": "string",
		"resolution": map[string]any{"rule": "literal", "default": "us-east-1"},
		"constraint": map[string]any{"enum": []any{"us-east-1", "us-west-2"}},
	}
}

// unconstrainedInput builds a raw input map declaring the given name with no
// constraint block.
func unconstrainedInput(name string) map[string]any {
	return map[string]any{
		"name": name, "type": "string",
		"resolution": map[string]any{"rule": "literal", "default": "anything"},
	}
}

// manifestWithInputsAndSteps builds a manifest directly (bypassing schema
// validation) so the guard's own rules are exercised as the freeze-time
// backstop, not a re-run of schema validation.
func manifestWithInputsAndSteps(inputs, steps []any) *manifest.Manifest {
	return &manifest.Manifest{
		Name:    "guardtest",
		Aileron: manifest.AileronBlock{Inputs: inputs, Steps: steps},
	}
}

// toolStepWithCommand builds a raw tool step whose argv is the given elements.
func toolStepWithCommand(id string, argv ...string) map[string]any {
	cmd := make([]any, len(argv))
	for i, a := range argv {
		cmd[i] = a
	}
	return map[string]any{"id": id, "kind": "tool", "command": cmd, "outputs": []any{"out"}}
}

// TestLint_CommandUnconstrainedInputRejected proves a tool command
// interpolating an UNCONSTRAINED input fails the freeze closed: an unconstrained
// value in a sealed exec position is an injection surface.
func TestLint_CommandUnconstrainedInputRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{unconstrainedInput("region")},
		[]any{toolStepWithCommand("q", "query", "--region={{ inputs.region }}")},
	)
	err := Lint(m)
	if err == nil {
		t.Fatal("a command referencing an unconstrained input must be rejected")
	}
	if !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error should explain the constraint requirement, got: %v", err)
	}
}

// TestLint_CommandConstrainedInputAccepted proves a tool command interpolating
// a CONSTRAINED input lints clean.
func TestLint_CommandConstrainedInputAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("region")},
		[]any{toolStepWithCommand("q", "query", "--region={{ inputs.region }}")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a command referencing a constrained input must lint clean: %v", err)
	}
}

// TestLint_CommandUnknownInputRejected proves a token naming an input that is
// not declared at freeze is rejected.
func TestLint_CommandUnknownInputRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("region")},
		[]any{toolStepWithCommand("q", "query", "--zone={{ inputs.zone }}")},
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "undeclared input") {
		t.Fatalf("a command referencing an unknown input must be rejected, got: %v", err)
	}
}

// TestLint_CommandMalformedTokenRejected proves a malformed brace shape in a
// tool command is rejected.
func TestLint_CommandMalformedTokenRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("region")},
		[]any{toolStepWithCommand("q", "query", "--region={{ inputs.region }")},
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a malformed command token must be rejected, got: %v", err)
	}
}

// TestLint_CommandTokenFreeClean proves a tool command with no interpolation
// lints clean regardless of input constraints (the regression that existing
// token-free plans are unaffected).
func TestLint_CommandTokenFreeClean(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{unconstrainedInput("region")},
		[]any{toolStepWithCommand("q", "extract-tool", "--mode", "csv")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a token-free tool command must lint clean: %v", err)
	}
}

// TestLint_CommandPatternConstraintAccepted proves a pattern constraint (not
// only an enum) counts as constrained for the guard.
func TestLint_CommandPatternConstraintAccepted(t *testing.T) {
	in := map[string]any{
		"name": "region", "type": "string",
		"resolution": map[string]any{"rule": "literal", "default": "us-east-1"},
		"constraint": map[string]any{"pattern": "^[a-z]{2}-[a-z]+-[0-9]$"},
	}
	m := manifestWithInputsAndSteps(
		[]any{in},
		[]any{toolStepWithCommand("q", "query", "--region={{ inputs.region }}")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a pattern-constrained input must satisfy the guard: %v", err)
	}
}

// interpToolStepMD is a full valid SKILL.md whose tool command interpolates a
// constrained input, used to prove freeze seals the TEMPLATE argv verbatim.
const interpToolStepMD = `---
name: interp-skill
description: A skill whose tool command interpolates a constrained input.
aileron:
  schemaVersion: aileron.flightplan.v1
  environment:
    tools:
      - aws-cli@2.x
  inputs:
    - name: region
      type: string
      resolution:
        rule: literal
        default: us-east-1
      constraint:
        enum:
          - us-east-1
          - us-west-2
  outputs: []
  steps:
    - id: fetch
      kind: tool
      command:
        - aws
        - s3
        - ls
        - "--region={{ inputs.region }}"
      outputs:
        - listing
      trustContract:
        credential:
          kind: none
        hosts:
          - s3.amazonaws.com
        effect: read
        idempotency:
          safeToRetry: true
        audit:
          fields:
            - result
---

# Interp Skill
`

// TestRun_SealsTemplateCommandNotResolved proves freeze seals the TEMPLATE
// argv: the frozen manifest still carries the `{{ inputs.region }}` token, not
// a resolved value. Instantiation is a launch-time step, never a freeze-time
// rewrite.
func TestRun_SealsTemplateCommandNotResolved(t *testing.T) {
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(interpToolStepMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer:       fakeComposer(fakeDigest2),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.FrozenManifest), "{{ inputs.region }}") {
		t.Errorf("frozen manifest must carry the TEMPLATE command token, not a resolved value:\n%s", res.FrozenManifest)
	}
	if strings.Contains(string(res.FrozenManifest), "--region=us-east-1") {
		t.Error("freeze must not instantiate the command; the resolved value must not appear in the sealed bytes")
	}
}
