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

// exampleSkillMD reads the committed worked example SKILL.md. It declares an
// environment with curated tools, so it exercises the composed-environment
// freeze path.
func exampleSkillMD(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
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

// envImageMD is a minimal valid manifest whose environment names a custom
// base image (the escape hatch with no curated tools to compose).
const envImageMD = `---
name: env-image-skill
description: A skill naming a custom base image.
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
    image: registry.example.com/runner:1.4
  inputs: []
  outputs: []
---

# Env Image Skill
`

// noEnvironmentMD is a valid manifest with an aileron block but no
// environment block (composition-only, no images to resolve).
const noEnvironmentMD = `---
name: no-environment
description: A skill with an aileron block but no environment.
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

# No Environment
`

// toolStepsMD is a valid manifest declaring environment tools and three tool
// steps: two with a trust contract (declared reach freeze seals into
// lock.stepTrust) and one without (omitted from the sealed section).
const toolStepsMD = `---
name: tool-steps-skill
description: A skill running declared environment tools with sealed reach.
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
      - gh@2
  inputs: []
  outputs: []
  steps:
    - id: fetch
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
          - s3.amazonaws.com:443
        effect: read
        idempotency:
          safeToRetry: true
        audit:
          fields:
            - result
    - id: version
      kind: tool
      command:
        - aws
        - --version
      outputs:
        - version
    - id: file
      kind: tool
      command:
        - gh
        - issue
        - create
      outputs:
        - issue
      trustContract:
        credential:
          kind: none
        hosts:
          - api.github.com
        effect: write
        idempotency:
          safeToRetry: false
        audit:
          fields:
            - result
---

# Tool Steps Skill
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
