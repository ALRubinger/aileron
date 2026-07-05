package freeze

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// toolStepWithHosts builds a raw tool step whose trustContract declares the
// given host entries (which may be templates). It carries a minimal valid
// contract shape so the host guard, not a malformed-contract path, is what the
// test exercises.
func toolStepWithHosts(id string, hosts ...string) map[string]any {
	hs := make([]any, len(hosts))
	for i, h := range hosts {
		hs[i] = h
	}
	return map[string]any{
		"id": id, "kind": "tool",
		"command": []any{"aws", "s3", "ls"},
		"outputs": []any{"out"},
		"trustContract": map[string]any{
			"credential":  map[string]any{"kind": "none"},
			"hosts":       hs,
			"effect":      "read",
			"idempotency": map[string]any{"safeToRetry": true},
			"audit":       map[string]any{"fields": []any{"result"}},
		},
	}
}

// TestLint_HostUnconstrainedInputRejected proves a tool trustContract host
// interpolating an UNCONSTRAINED input fails the freeze closed: an
// unconstrained value in a sealed host position could steer the step's egress.
func TestLint_HostUnconstrainedInputRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{unconstrainedInput("aws_region")},
		[]any{toolStepWithHosts("q", "athena.{{ inputs.aws_region }}.amazonaws.com")},
	)
	err := Lint(m)
	if err == nil {
		t.Fatal("a host referencing an unconstrained input must be rejected")
	}
	if !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error should explain the constraint requirement, got: %v", err)
	}
}

// TestLint_HostConstrainedInputAccepted proves a tool trustContract host
// interpolating a CONSTRAINED input lints clean.
func TestLint_HostConstrainedInputAccepted(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("aws_region")},
		[]any{toolStepWithHosts("q", "athena.{{ inputs.aws_region }}.amazonaws.com")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a host referencing a constrained input must lint clean: %v", err)
	}
}

// TestLint_HostUnknownInputRejected proves a host token naming an input not
// declared at freeze is rejected.
func TestLint_HostUnknownInputRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("aws_region")},
		[]any{toolStepWithHosts("q", "athena.{{ inputs.zone }}.amazonaws.com")},
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "undeclared input") {
		t.Fatalf("a host referencing an unknown input must be rejected, got: %v", err)
	}
}

// TestLint_HostMalformedTokenRejected proves a malformed brace shape in a host
// is rejected.
func TestLint_HostMalformedTokenRejected(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("aws_region")},
		[]any{toolStepWithHosts("q", "athena.{{ inputs.aws_region }.amazonaws.com")},
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a malformed host token must be rejected, got: %v", err)
	}
}

// TestLint_HostTokenFreeClean proves a token-free host lints clean regardless
// of input constraints (existing non-templated plans are unaffected).
func TestLint_HostTokenFreeClean(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{unconstrainedInput("aws_region")},
		[]any{toolStepWithHosts("q", "s3.amazonaws.com")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a token-free host must lint clean: %v", err)
	}
}

// TestLint_HostNoTrustContractClean proves a tool step that declares NO
// trustContract is skipped by the host guard (it declares no host reach) and
// lints clean — the guard never synthesizes a reach for a contract-less step.
func TestLint_HostNoTrustContractClean(t *testing.T) {
	m := manifestWithInputsAndSteps(
		[]any{constrainedInput("aws_region")},
		[]any{map[string]any{"id": "q", "kind": "tool", "command": []any{"aws"}, "outputs": []any{"out"}}},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a tool step with no trustContract must lint clean: %v", err)
	}
}

// TestLint_HostMalformedContractSkipped proves the host guard tolerates a
// trustContract whose hosts are missing, non-array, or carry a non-string
// entry: those shapes are schema and steptrust-seal errors caught elsewhere, so
// the host guard skips them (a present host STRING is all it inspects) rather
// than double-reporting a shape the sealing path already refuses.
func TestLint_HostMalformedContractSkipped(t *testing.T) {
	base := func(tc map[string]any) *manifest.Manifest {
		return manifestWithInputsAndSteps(
			[]any{constrainedInput("aws_region")},
			[]any{map[string]any{"id": "q", "kind": "tool", "command": []any{"aws"}, "outputs": []any{"out"}, "trustContract": tc}},
		)
	}
	cases := map[string]map[string]any{
		"hosts key absent":      {"credential": map[string]any{"kind": "none"}, "effect": "read"},
		"hosts not an array":    {"hosts": "s3.amazonaws.com"},
		"non-string host entry": {"hosts": []any{42}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Lint(base(tc)); err != nil {
				t.Errorf("the host guard must skip a malformed contract shape, got: %v", err)
			}
		})
	}
}

// TestLint_HostPatternConstraintAccepted proves a pattern constraint (not only
// an enum) counts as constrained for the host guard.
func TestLint_HostPatternConstraintAccepted(t *testing.T) {
	in := map[string]any{
		"name": "aws_region", "type": "string",
		"resolution": map[string]any{"rule": "literal", "default": "us-east-1"},
		"constraint": map[string]any{"pattern": "^[a-z]{2}-[a-z]+-[0-9]$"},
	}
	m := manifestWithInputsAndSteps(
		[]any{in},
		[]any{toolStepWithHosts("q", "athena.{{ inputs.aws_region }}.amazonaws.com")},
	)
	if err := Lint(m); err != nil {
		t.Errorf("a pattern-constrained input must satisfy the host guard: %v", err)
	}
}

// interpHostToolStepMD is a full valid SKILL.md whose tool trustContract host
// interpolates a constrained input, used to prove freeze seals the TEMPLATE
// host verbatim into lock.stepTrust.
const interpHostToolStepMD = `---
name: interp-host-skill
description: A skill whose tool trustContract host interpolates a constrained input.
aileron:
  schemaVersion: aileron.flightplan.v1
  environment:
    tools:
      - aws-cli@2.x
  inputs:
    - name: aws_region
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
        - athena
        - list-databases
      outputs:
        - listing
      trustContract:
        credential:
          kind: none
        hosts:
          - athena.{{ inputs.aws_region }}.amazonaws.com
        effect: read
        idempotency:
          safeToRetry: true
        audit:
          fields:
            - result
---

# Interp Host Skill
`

// TestRun_SealsTemplateHostNotResolved proves freeze seals the TEMPLATE host:
// both the frozen frontmatter and the lock.stepTrust bytes still carry the
// `{{ inputs.aws_region }}` token, not a resolved value. Instantiation is a
// launch-time step, never a freeze-time rewrite; sealing the template is what
// makes the token bytes part of the attestation.
func TestRun_SealsTemplateHostNotResolved(t *testing.T) {
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(interpHostToolStepMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer:       fakeComposer(fakeDigest2),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	frozen := string(res.FrozenManifest)
	if !strings.Contains(frozen, "athena.{{ inputs.aws_region }}.amazonaws.com") {
		t.Errorf("frozen manifest must carry the TEMPLATE host token, not a resolved value:\n%s", frozen)
	}
	if strings.Contains(frozen, "athena.us-east-1.amazonaws.com") {
		t.Error("freeze must not instantiate the host; the resolved value must not appear in the sealed bytes")
	}
	// The sealed reach in lock.stepTrust must also carry the template verbatim:
	// the lock section is the reach the runtime consumes and is covered by the
	// signature, so the token must ride there too.
	if lock := string(res.Lockfile); !strings.Contains(lock, "athena.{{ inputs.aws_region }}.amazonaws.com") {
		t.Errorf("lock.stepTrust must seal the TEMPLATE host verbatim:\n%s", lock)
	}
}

// manifestWithActions builds a manifest whose requires.actions carries the given
// ActionRequirements directly (bypassing schema validation) so the per-action
// host guard's own rule is exercised as the freeze-time backstop.
func manifestWithActions(actions ...manifest.ActionRequirement) *manifest.Manifest {
	return &manifest.Manifest{
		Name: "actionhostguard",
		Aileron: manifest.AileronBlock{
			Requires: manifest.Requires{Actions: actions},
		},
	}
}

// actionWithHosts builds a per-action requirement whose trustContract declares
// the given host entries (which may be templates).
func actionWithHosts(ref string, hosts ...string) manifest.ActionRequirement {
	hs := make([]any, len(hosts))
	for i, h := range hosts {
		hs[i] = h
	}
	return manifest.ActionRequirement{
		Ref: ref,
		TrustContract: map[string]any{
			"credential":  map[string]any{"kind": "none"},
			"hosts":       hs,
			"effect":      "read",
			"idempotency": map[string]any{"safeToRetry": true},
			"audit":       map[string]any{"fields": []any{"result"}},
		},
	}
}

// TestLint_ActionHostInputTokenRejected proves an input token in a PER-ACTION
// (requires.actions[]) trustContract host fails the freeze closed (#1965). Host
// interpolation is tool-step-only, so such a token is never substituted and
// would ride in inert as the literal `{{ ... }}` string, silently granting a
// reach that is syntactically valid but wrong. Per-action hosts must be literal.
func TestLint_ActionHostInputTokenRejected(t *testing.T) {
	m := manifestWithActions(
		actionWithHosts("aws/athena@1.0.0", "athena.{{ inputs.aws_region }}.amazonaws.com"),
	)
	err := Lint(m)
	if err == nil {
		t.Fatal("a per-action host referencing an input token must be rejected")
	}
	if !strings.Contains(err.Error(), "tool-step-only") {
		t.Errorf("error should explain host interpolation is tool-step-only, got: %v", err)
	}
}

// TestLint_ActionHostLiteralAccepted proves a per-action trustContract host that
// is a literal (no input token) lints clean — existing action-ref plans with
// literal reach are unaffected.
func TestLint_ActionHostLiteralAccepted(t *testing.T) {
	m := manifestWithActions(
		actionWithHosts("aws/athena@1.0.0", "athena.us-east-1.amazonaws.com"),
	)
	if err := Lint(m); err != nil {
		t.Errorf("a literal per-action host must lint clean: %v", err)
	}
}

// TestLint_ActionHostMalformedTokenRejected proves a malformed brace shape in a
// per-action host is rejected by the shared token grammar, not silently sealed.
func TestLint_ActionHostMalformedTokenRejected(t *testing.T) {
	m := manifestWithActions(
		actionWithHosts("aws/athena@1.0.0", "athena.{{ inputs.aws_region }.amazonaws.com"),
	)
	err := Lint(m)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a malformed per-action host token must be rejected, got: %v", err)
	}
}
