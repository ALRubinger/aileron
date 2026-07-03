package freeze

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// repoRoot walks up to the directory containing go.work so tests can read
// the committed worked example as a known-valid credentialed skill.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.work from %s", filepath.Dir(file))
		}
		dir = parent
	}
}

// exampleSkillMD reads the committed rung-2 worked example SKILL.md.
func exampleSkillMD(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	return raw
}

// exampleRung1SkillMD reads the committed rung-1 worked example SKILL.md: a
// skill naming a whole prebuilt image (rung1Image.ref) with no capability
// units to compose. It is the living-documentation parallel to the rung-2
// example.
func exampleRung1SkillMD(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.rung1.skill.md"))
	if err != nil {
		t.Fatalf("read rung-1 worked example: %v", err)
	}
	return raw
}

// instructionOnlyMD is a SKILL.md with no aileron block.
const instructionOnlyMD = `---
name: rubber-duck
description: Instruction-only skill.
---

# Rubber Duck
Explain it out loud.
`

// rung1MD is a minimal valid rung-1 manifest (named image to resolve).
const rung1MD = `---
name: rung1-skill
description: A rung-1 skill naming a prebuilt image.
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
      rung1Image:
        ref: registry.example.com/runner:1.4
  inputs: []
  outputs: []
---

# Rung 1 Skill
`

// rung1DefaultMD is a minimal valid rung-1 manifest that declares no ref
// under rung1Image, so freeze resolves the Aileron-provided default runner
// image for the CLI version (#1808). It exercises the Unit-1 schema through
// manifest.Parse (rung1Image: {} is valid).
const rung1DefaultMD = `---
name: rung1-default-skill
description: A rung-1 skill using the default Aileron runner image.
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
      rung1Image: {}
  inputs: []
  outputs: []
---

# Rung 1 Default Skill
`

// noExecEnvMD is a valid manifest with an aileron block but no
// executionEnvironment (composition-only, no images to resolve).
const noExecEnvMD = `---
name: no-exec-env
description: A skill with an aileron block but no execution environment.
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

# No Exec Env
`

// rung3MD declares a single-step rung-3 execution environment
// (rung3PerStepImages) with neither rung-1 nor rung-2. Rung three is a built
// image rung: freeze resolves each per-step sibling image to a digest pin
// (ADR-0027, #1732).
const rung3MD = `---
name: rung3-skill
description: A skill declaring a rung-3 per-step image.
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
      rung3PerStepImages:
        steps:
          - image: registry.example.com/per-step-tool:1
  inputs: []
  outputs: []
---

# Rung 3 Skill
`

// rung3MultiStepMD declares a rung-3 execution environment with two steps
// (each naming a distinct sibling image) plus optional id/mount/collect I/O.
// It proves freeze pins one digest per step and preserves declared order.
const rung3MultiStepMD = `---
name: rung3-multistep-skill
description: A skill declaring multiple rung-3 per-step images.
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
      rung3PerStepImages:
        steps:
          - id: extract
            image: registry.example.com/tool-a:1
            mount:
              path: /work
            collect:
              path: /work/out
          - id: convert
            image: registry.example.com/tool-b:2
  inputs: []
  outputs: []
---

# Rung 3 Multi-Step Skill
`

// rung3SharedTagMD declares two rung-3 steps that name the SAME image tag but
// carry distinct declared ids. It is the #1739 tag-collision fixture: freeze
// must pin each step distinctly by its id, never collapse the two steps onto a
// single ref-keyed pin.
const rung3SharedTagMD = `---
name: rung3-shared-tag-skill
description: Two rung-3 steps sharing one image tag.
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
      rung3PerStepImages:
        steps:
          - id: first
            image: registry.example.com/shared-tool:1
          - id: second
            image: registry.example.com/shared-tool:1
  inputs: []
  outputs: []
---

# Rung 3 Shared Tag Skill
`

// rung3NoIDMD declares two rung-3 steps with NO declared id, so freeze must
// stamp a positional fallback id onto each pin.
const rung3NoIDMD = `---
name: rung3-no-id-skill
description: Rung-3 steps with no declared id.
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
      rung3PerStepImages:
        steps:
          - image: registry.example.com/tool-a:1
          - image: registry.example.com/tool-b:2
  inputs: []
  outputs: []
---

# Rung 3 No ID Skill
`

// rung3TrustContractMD declares two rung-3 steps: the first carries a per-step
// trustContract whose hosts declare the step's network reach; the second
// declares no trustContract (no reach). Freeze must seal the first step's hosts
// onto its pin and leave the second pin's hosts empty (#1775).
const rung3TrustContractMD = `---
name: rung3-trust-contract-skill
description: A rung-3 skill with a per-step trust contract.
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
      rung3PerStepImages:
        steps:
          - id: reach
            image: registry.example.com/tool-a:1
            trustContract:
              credential:
                kind: none
              hosts:
                - api.upstream.example.com
                - api.upstream.example.com:443
              effect: read
              idempotency:
                safeToRetry: true
              audit:
                fields:
                  - result
          - id: noreach
            image: registry.example.com/tool-b:2
  inputs: []
  outputs: []
---

# Rung 3 Trust Contract Skill
`

// genSigningKey returns a fresh ed25519 private key and writes its PKCS#8
// PEM form to a temp file, returning the path. Mirrors the keyring PEM
// conventions; used to exercise LoadSigningKey + Sign.
func genSigningKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "signing-key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return priv, path
}

// fakeDigest is a valid sha256 digest for tests.
const fakeDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const fakeDigest2 = "sha256:" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
