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

// execEnvFrontmatter wraps an executionEnvironment block body into a complete,
// otherwise-valid aileron frontmatter so a test exercises only the
// executionEnvironment validity rules. envBlock is the YAML for the
// executionEnvironment value, indented to sit under requires.
func execEnvFrontmatter(envBlock string) []byte {
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
` + envBlock + `
  inputs: []
  outputs: []
`)
}

// TestExecutionEnvironmentRungValidity locks the rung composition rules at the
// schema boundary: rung-1-only, rung-2-only, and rung-3-only are each valid,
// and the three rungs are mutually exclusive. Declaring any two together is
// rejected (rung-3 is now a built image rung, so it excludes rung-1 and rung-2
// just as they exclude each other). A malformed rung-3 (empty steps) is
// rejected. A present-but-empty executionEnvironment ({}) is rejected:
// declaring the key obliges naming exactly one rung (key omission, not an empty
// block, is how a skill says it has no execution environment).
func TestExecutionEnvironmentRungValidity(t *testing.T) {
	valid := map[string]string{
		"rung1 only": `      rung1Image:
        ref: registry.example.com/runner:1.4`,
		"rung2 only": `      rung2CapabilityUnits:
        features:
          - ghcr.io/example/aileron-feature-metrics-cli:1`,
		"rung3 only": `      rung3PerStepImages:
        steps:
          - image: registry.example.com/per-step-tool:1`,
		"rung3 with id and io": `      rung3PerStepImages:
        steps:
          - id: convert
            image: registry.example.com/per-step-tool:1
            mount:
              path: /work
            collect:
              path: /work/out`,
	}
	for name, env := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := validateFrontmatter(execEnvFrontmatter(env)); err != nil {
				t.Fatalf("expected valid, got: %v", err)
			}
		})
	}

	invalid := map[string]string{
		"rung1 and rung2 together rejected": `      rung1Image:
        ref: registry.example.com/runner:1.4
      rung2CapabilityUnits:
        features:
          - ghcr.io/example/aileron-feature-metrics-cli:1`,
		"rung3 alongside rung1 rejected": `      rung1Image:
        ref: registry.example.com/runner:1.4
      rung3PerStepImages:
        steps:
          - image: registry.example.com/per-step-tool:1`,
		"rung3 with empty steps rejected": `      rung3PerStepImages:
        steps: []`,
		"empty executionEnvironment rejected": `      {}`,
	}
	for name, env := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			err := validateFrontmatter(execEnvFrontmatter(env))
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "schema validation") {
				t.Errorf("error should cite schema validation, got: %v", err)
			}
		})
	}
}
