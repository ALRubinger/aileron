package manifest

import (
	"strings"
	"testing"
)

// invalidCases enumerates the contract's stated failure modes for a
// present aileron block. Each document has the block present (so the parser
// must validate it) but violates exactly one required-field rule.
func TestParseInvalidBlocks(t *testing.T) {
	cases := map[string]string{
		"missing requires": `---
name: s
description: d
aileron:
  inputs: []
  outputs: []
---
body
`,
		"missing inputs": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract: {}
  outputs: []
---
body
`,
		"missing outputs": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract: {}
  inputs: []
---
body
`,
		"empty actions array": `---
name: s
description: d
aileron:
  requires:
    actions: []
  inputs: []
  outputs: []
---
body
`,
		"action missing ref": `---
name: s
description: d
aileron:
  requires:
    actions:
      - trustContract: {}
  inputs: []
  outputs: []
---
body
`,
		"action missing trustContract": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: aileron:metrics.query_series
  inputs: []
  outputs: []
---
body
`,
		"ref malformed (no connector.action)": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: not-a-valid-ref
        trustContract: {}
  inputs: []
  outputs: []
---
body
`,
	}

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "schema validation") {
				t.Errorf("error should cite schema validation, got: %v", err)
			}
		})
	}
}

// TestStrippedTopLevelStillValid confirms the schema does not reject a
// document with the aileron block absent. This is the lossless-if-stripped
// guarantee at the parser boundary: an instruction-only skill is valid.
func TestStrippedTopLevelStillValid(t *testing.T) {
	if err := validateFrontmatter([]byte("name: s\ndescription: d\n")); err != nil {
		t.Fatalf("a stripped manifest must validate, got: %v", err)
	}
}

// lockFrontmatter wraps a lock block body into a complete, otherwise-valid
// aileron frontmatter so a test exercises only the lock validity rules.
// lockBlock is the YAML for the lock value's keys, indented to sit under
// `lock:` (4 spaces for its keys).
func lockFrontmatter(lockBlock string) []byte {
	return []byte(`name: s
description: d
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
  inputs: []
  outputs: []
  lock:
` + lockBlock + `
`)
}

const lockTestDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestLockResolvedImagesSinglePinShape locks the one-pin model at the schema
// boundary (#1827): a resolvedImages item is exactly {ref, digest}. The
// retired per-pin `id` and `hosts` keys (the rung-3 stepId-on-pin linkage) are
// rejected outright; the step-keyed sealed reach lives in the sibling
// stepTrust section instead.
func TestLockResolvedImagesSinglePinShape(t *testing.T) {
	item := func(extra string) string {
		return "    resolvedImages:\n      - ref: registry.example.com/runner:1.4\n        digest: " + lockTestDigest + extra
	}

	t.Run("valid/ref and digest only", func(t *testing.T) {
		if err := validateFrontmatter(lockFrontmatter(item(""))); err != nil {
			t.Fatalf("a ref+digest pin must validate, got: %v", err)
		}
	})
	t.Run("invalid/per-pin id rejected", func(t *testing.T) {
		if err := validateFrontmatter(lockFrontmatter(item("\n        id: extract"))); err == nil {
			t.Fatal("the retired per-pin id key must be rejected")
		}
	})
	t.Run("invalid/per-pin hosts rejected", func(t *testing.T) {
		if err := validateFrontmatter(lockFrontmatter(item("\n        hosts:\n          - api.example.com"))); err == nil {
			t.Fatal("the retired per-pin hosts key must be rejected")
		}
	})
	t.Run("invalid/unknown key", func(t *testing.T) {
		if err := validateFrontmatter(lockFrontmatter(item("\n        stepId: extract"))); err == nil {
			t.Fatal("an unknown key on a resolvedImages item must be rejected")
		}
	})
}

// TestLockStepTrust locks the step-keyed sealed trust section at the schema
// boundary (#1827): stepTrust is keyed by tool-step id, each entry requires a
// non-empty hosts array using the trust-contract host pattern, and the entry
// object is closed.
func TestLockStepTrust(t *testing.T) {
	valid := map[string]string{
		"single step": `    stepTrust:
      fetch:
        hosts:
          - s3.amazonaws.com`,
		"multiple steps with ports": `    stepTrust:
      fetch:
        hosts:
          - s3.amazonaws.com
          - s3.amazonaws.com:443
      file:
        hosts:
          - tracker.example.com`,
		"alongside a pin": `    resolvedImages:
      - ref: registry.example.com/runner:1.4
        digest: ` + lockTestDigest + `
    stepTrust:
      fetch:
        hosts:
          - api.example.com`,
	}
	for name, lock := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := validateFrontmatter(lockFrontmatter(lock)); err != nil {
				t.Fatalf("expected valid, got: %v", err)
			}
		})
	}

	invalid := map[string]string{
		"empty hosts": `    stepTrust:
      fetch:
        hosts: []`,
		"missing hosts": `    stepTrust:
      fetch: {}`,
		"scheme-prefixed host": `    stepTrust:
      fetch:
        hosts:
          - https://api.example.com`,
		"unknown key on entry": `    stepTrust:
      fetch:
        hosts:
          - api.example.com
        effect: read`,
		"non-object entry": `    stepTrust:
      fetch: s3.amazonaws.com`,
		"key not a step id": `    stepTrust:
      "not a step id":
        hosts:
          - api.example.com`,
	}
	for name, lock := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			err := validateFrontmatter(lockFrontmatter(lock))
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "schema validation") {
				t.Errorf("error should cite schema validation, got: %v", err)
			}
		})
	}
}

// environmentFrontmatter wraps an `environment` block body into a complete,
// otherwise-valid aileron frontmatter so a test exercises only the
// environment validity rules. envBlock is the YAML for the environment
// value, indented to sit under the aileron block (4 spaces for its keys).
func environmentFrontmatter(envBlock string) []byte {
	return []byte(`name: s
description: d
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
  environment:
` + envBlock + `
  inputs: []
  outputs: []
`)
}

// TestEnvironmentValidity locks the environment contract at the schema
// boundary. A present block must declare tools (curated-catalog refs), an
// image (the custom-base escape hatch), or both; the tool-name vocabulary is
// closed to the curated catalog, so an unknown or malformed tool ref is
// rejected at validation, before freeze. An empty environment ({}) is
// rejected: omitting the key, not an empty block, is how a skill says it has
// no environment needs. The version grammar is deliberately loose (2, 2.x,
// 2.19.1 all pass); resolution against the catalog is freeze's job.
func TestEnvironmentValidity(t *testing.T) {
	valid := map[string]string{
		"tools only": `    tools:
      - aws-cli@2.x`,
		"multiple tools": `    tools:
      - aws-cli@2.x
      - gh@2`,
		"loose version grammar": `    tools:
      - aws-cli@2
      - gh@2.19.1`,
		"image only": `    image: registry.example.com/base:1.4`,
		"image and tools": `    image: registry.example.com/base:1.4
    tools:
      - gh@2`,
	}
	for name, env := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := validateFrontmatter(environmentFrontmatter(env)); err != nil {
				t.Fatalf("expected valid, got: %v", err)
			}
		})
	}

	invalid := map[string]string{
		"empty environment rejected": `    {}`,
		"unknown tool name rejected": `    tools:
      - nmap@7.1`,
		"tool without version rejected": `    tools:
      - aws-cli`,
		"version without name rejected": `    tools:
      - "@2.x"`,
		"empty tool entry rejected": `    tools:
      - ""`,
		"empty version rejected": `    tools:
      - aws-cli@`,
		"duplicate tools rejected": `    tools:
      - gh@2
      - gh@2`,
		"empty tools array rejected": `    tools: []`,
		"empty image rejected":       `    image: ""`,
		"non-string image rejected":  `    image: 7`,
		"rung1Image under environment rejected": `    rung1Image:
      ref: registry.example.com/runner:1.4`,
		"rung2CapabilityUnits under environment rejected": `    rung2CapabilityUnits:
      features:
        - ghcr.io/example/aileron-feature-metrics-cli:1`,
		"rung3PerStepImages under environment rejected": `    rung3PerStepImages:
      steps:
        - image: registry.example.com/per-step-tool:1`,
	}
	for name, env := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			err := validateFrontmatter(environmentFrontmatter(env))
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "schema validation") {
				t.Errorf("error should cite schema validation, got: %v", err)
			}
		})
	}
}

// TestExecutionEnvironmentKeyRemoved locks the contract break: the retired
// `requires.executionEnvironment` key is rejected outright, whichever rung
// shape it carries. There is no compatibility window; the environment block
// replaced the three-rung oneOf.
func TestExecutionEnvironmentKeyRemoved(t *testing.T) {
	withExecEnv := func(env string) []byte {
		return []byte(`name: s
description: d
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
    executionEnvironment:
` + env + `
  inputs: []
  outputs: []
`)
	}
	cases := map[string]string{
		"rung1Image": `      rung1Image:
        ref: registry.example.com/runner:1.4`,
		"rung2CapabilityUnits": `      rung2CapabilityUnits:
        features:
          - ghcr.io/example/aileron-feature-metrics-cli:1`,
		"rung3PerStepImages": `      rung3PerStepImages:
        steps:
          - image: registry.example.com/per-step-tool:1`,
		"empty": `      {}`,
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateFrontmatter(withExecEnv(env)); err == nil {
				t.Fatalf("a lingering requires.executionEnvironment (%s) must be rejected", name)
			}
		})
	}
}

// TestParsePopulatesEnvironment proves the typed model carries the declared
// environment: tools and image land on AileronBlock.Environment, and a
// manifest with no environment block parses with a nil Environment.
func TestParsePopulatesEnvironment(t *testing.T) {
	doc := environmentFrontmatter(`    image: registry.example.com/base:1.4
    tools:
      - aws-cli@2.x
      - gh@2`)
	full := append([]byte("---\n"), doc...)
	full = append(full, []byte("---\nbody\n")...)
	m, err := Parse(full)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	env := m.Aileron.Environment
	if env == nil {
		t.Fatal("Environment must be populated when the block is declared")
	}
	if env.Image != "registry.example.com/base:1.4" {
		t.Errorf("Image = %q", env.Image)
	}
	if strings.Join(env.Tools, ",") != "aws-cli@2.x,gh@2" {
		t.Errorf("Tools = %v", env.Tools)
	}

	noEnv := `---
name: s
description: d
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
  inputs: []
  outputs: []
---
body
`
	m2, err := Parse([]byte(noEnv))
	if err != nil {
		t.Fatalf("parse no-environment manifest: %v", err)
	}
	if m2.Aileron.Environment != nil {
		t.Errorf("a manifest with no environment block must carry a nil Environment, got %+v", m2.Aileron.Environment)
	}
}

// toolStepFrontmatter wraps a steps block body into a complete, otherwise
// valid aileron frontmatter (with an environment declaring the tool) so a
// test exercises only the tool-step validity rules. stepsBlock is the YAML
// for the steps array items, indented to sit under `steps:`.
func toolStepFrontmatter(stepsBlock string) []byte {
	return []byte(`name: s
description: d
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
  environment:
    tools:
      - aws-cli@2.x
  inputs: []
  outputs: []
  steps:
` + stepsBlock + `
`)
}

// TestToolStepValidity locks the tool step kind at the schema boundary. A
// tool step requires an argv-array command (never a shell string), may carry
// bindings, mount/collect I/O, outputs, and an optional trust contract, and
// stays a closed object. Every other step kind's closed shape forbids the
// `command` key, so a command cannot ride on an action-call, transform, or
// llm-seam.
func TestToolStepValidity(t *testing.T) {
	valid := map[string]string{
		"command with outputs and trust contract": `    - id: fetch
      kind: tool
      command:
        - aws
        - s3
        - ls
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
            - result`,
		"mount and collect": `    - id: convert
      kind: tool
      command:
        - aws
        - s3
        - cp
        - /work/in
        - /work/out
      mount:
        path: /work/in
      collect:
        path: /work/out
      outputs:
        - copied`,
		"bindings and materializesOutput": `    - id: render
      kind: tool
      command:
        - aws
        - --version
      bindings:
        window: inputs.window_days
      outputs:
        - version
      materializesOutput: report.csv`,
	}
	for name, steps := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := validateFrontmatter(toolStepFrontmatter(steps)); err != nil {
				t.Fatalf("expected valid, got: %v", err)
			}
		})
	}

	invalid := map[string]string{
		"missing command": `    - id: fetch
      kind: tool
      outputs:
        - listing`,
		"empty command array": `    - id: fetch
      kind: tool
      command: []`,
		"empty argv element": `    - id: fetch
      kind: tool
      command:
        - aws
        - ""`,
		"shell-string command": `    - id: fetch
      kind: tool
      command: aws s3 ls`,
		"empty mount path": `    - id: fetch
      kind: tool
      command:
        - aws
      mount:
        path: ""`,
		"mount without path": `    - id: fetch
      kind: tool
      command:
        - aws
      mount: {}`,
		"empty collect path": `    - id: fetch
      kind: tool
      command:
        - aws
      collect:
        path: ""`,
		"unknown key on tool step": `    - id: fetch
      kind: tool
      command:
        - aws
      image: registry.example.com/tool:1`,
		"trust contract with empty hosts": `    - id: fetch
      kind: tool
      command:
        - aws
      trustContract:
        credential:
          kind: none
        hosts: []
        effect: read
        idempotency:
          safeToRetry: true
        audit:
          fields:
            - result`,
		"command on a transform step": `    - id: reshape
      kind: transform
      command:
        - aws
      outputs:
        - out`,
		"command on an action-call step": `    - id: call
      kind: action-call
      actionRef: aileron:metrics.query_series
      command:
        - aws`,
		"command on an llm-seam step": `    - id: seam
      kind: llm-seam
      command:
        - aws
      outputs:
        - out`,
	}
	for name, steps := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			err := validateFrontmatter(toolStepFrontmatter(steps))
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "schema validation") {
				t.Errorf("error should cite schema validation, got: %v", err)
			}
		})
	}
}
