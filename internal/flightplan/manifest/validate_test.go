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

// TestLockResolvedImagesID locks the rung-3 association substrate at the schema
// boundary: a lock whose resolvedImages[] item carries the optional `id` is
// valid (the step→pin link), a lock item with only ref+digest stays valid
// (rung-1/rung-2 pins carry no id), and an unknown key on the item is still
// rejected (additionalProperties:false is preserved).
func TestLockResolvedImagesID(t *testing.T) {
	lockFrontmatter := func(item string) []byte {
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
    resolvedImages:
` + item + `
`)
	}
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("valid/with id", func(t *testing.T) {
		item := "      - ref: registry.example.com/tool-a:1\n        digest: " + digest + "\n        id: extract"
		if err := validateFrontmatter(lockFrontmatter(item)); err != nil {
			t.Fatalf("a resolvedImages item with id must validate, got: %v", err)
		}
	})
	t.Run("valid/without id", func(t *testing.T) {
		item := "      - ref: registry.example.com/runner:1.4\n        digest: " + digest
		if err := validateFrontmatter(lockFrontmatter(item)); err != nil {
			t.Fatalf("a resolvedImages item without id must validate, got: %v", err)
		}
	})
	t.Run("invalid/unknown key", func(t *testing.T) {
		item := "      - ref: registry.example.com/tool-a:1\n        digest: " + digest + "\n        stepId: extract"
		if err := validateFrontmatter(lockFrontmatter(item)); err == nil {
			t.Fatal("an unknown key on a resolvedImages item must be rejected")
		}
	})
	t.Run("invalid/empty id", func(t *testing.T) {
		item := "      - ref: registry.example.com/tool-a:1\n        digest: " + digest + "\n        id: \"\""
		if err := validateFrontmatter(lockFrontmatter(item)); err == nil {
			t.Fatal("an empty id (minLength:1) must be rejected")
		}
	})
}

// TestRung3StepTrustContract locks the per-step trust contract at the schema
// boundary (#1775): a rung-3 step may carry a trustContract reusing the
// per-action $def, so a declared hosts+effect validates, an empty hosts list
// (violating the $def hosts minItems:1) is rejected, an unknown effect is
// rejected, and an unknown key on the step item is still rejected
// (additionalProperties:false on the step item is preserved by admitting only
// the one trustContract key).
func TestRung3StepTrustContract(t *testing.T) {
	t.Run("valid/hosts and effect", func(t *testing.T) {
		env := `      rung3PerStepImages:
        steps:
          - id: reach
            image: registry.example.com/per-step-tool:1
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
                  - result`
		if err := validateFrontmatter(execEnvFrontmatter(env)); err != nil {
			t.Fatalf("a rung-3 step with a trustContract must validate, got: %v", err)
		}
	})
	t.Run("invalid/empty hosts", func(t *testing.T) {
		env := `      rung3PerStepImages:
        steps:
          - image: registry.example.com/per-step-tool:1
            trustContract:
              credential:
                kind: none
              hosts: []
              effect: read
              idempotency:
                safeToRetry: true
              audit:
                fields:
                  - result`
		if err := validateFrontmatter(execEnvFrontmatter(env)); err == nil {
			t.Fatal("a trustContract with empty hosts (minItems:1) must be rejected")
		}
	})
	t.Run("invalid/unknown effect", func(t *testing.T) {
		env := `      rung3PerStepImages:
        steps:
          - image: registry.example.com/per-step-tool:1
            trustContract:
              credential:
                kind: none
              hosts:
                - api.example.com
              effect: teleport
              idempotency:
                safeToRetry: true
              audit:
                fields:
                  - result`
		if err := validateFrontmatter(execEnvFrontmatter(env)); err == nil {
			t.Fatal("a trustContract with an unknown effect must be rejected")
		}
	})
	t.Run("invalid/unknown key on step item", func(t *testing.T) {
		env := `      rung3PerStepImages:
        steps:
          - image: registry.example.com/per-step-tool:1
            bogus: nope`
		if err := validateFrontmatter(execEnvFrontmatter(env)); err == nil {
			t.Fatal("an unknown key on a rung-3 step item must be rejected (additionalProperties:false)")
		}
	})
}

// TestLockResolvedImagesHosts locks the per-pin sealed reach at the schema
// boundary (#1775): a lock resolvedImages[] item may carry the optional
// `hosts` array (freeze's sealed reach), a malformed host (a scheme-prefixed
// entry) is rejected by the host pattern, and the item stays
// additionalProperties:false.
func TestLockResolvedImagesHosts(t *testing.T) {
	lockFrontmatter := func(item string) []byte {
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
    resolvedImages:
` + item + `
`)
	}
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("valid/with hosts", func(t *testing.T) {
		item := "      - ref: registry.example.com/tool-a:1\n        digest: " + digest +
			"\n        id: reach\n        hosts:\n          - api.example.com\n          - api.example.com:443"
		if err := validateFrontmatter(lockFrontmatter(item)); err != nil {
			t.Fatalf("a resolvedImages item with hosts must validate, got: %v", err)
		}
	})
	t.Run("invalid/scheme-prefixed host", func(t *testing.T) {
		item := "      - ref: registry.example.com/tool-a:1\n        digest: " + digest +
			"\n        hosts:\n          - https://api.example.com"
		if err := validateFrontmatter(lockFrontmatter(item)); err == nil {
			t.Fatal("a scheme-prefixed host must be rejected by the host pattern")
		}
	})
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
		"rung1 with no ref": `      rung1Image: {}`,
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
		"rung1 empty-string ref rejected": `      rung1Image:
        ref: ""`,
		"rung1 non-string ref rejected": `      rung1Image:
        ref: 7`,
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
