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

// rung3MD declares only the reserved, build-deferred rung-3 slot
// (rung3PerStepImages) with neither rung-1 nor rung-2. The schema permits a
// rung-3-only declaration and freeze parses it, builds no image, and reports
// it as deferred (ADR-0027).
const rung3MD = `---
name: rung3-skill
description: A skill declaring only the reserved rung-3 slot.
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
