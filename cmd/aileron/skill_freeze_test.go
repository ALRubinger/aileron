package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"
	"github.com/ALRubinger/aileron/internal/sandbox/container"
)

// dockerInspectJSON builds a `docker image inspect --format '{{json .}}'` body
// for an image whose Entrypoint carries marker, so distinct markers yield
// distinct config content digests. It is the local-side input the composer and
// launch resolver canonicalize.
func dockerInspectJSON(t *testing.T, marker string) string {
	t.Helper()
	obj := map[string]any{
		"Id":           "sha256:" + strings.Repeat("0", 64),
		"Os":           "linux",
		"Architecture": "amd64",
		"Config": map[string]any{
			"Env":        []string{"PATH=/usr/bin"},
			"Entrypoint": []string{"/entry", marker},
			"Cmd":        []string{"bash"},
			"WorkingDir": "/work",
		},
		"RootFS": map[string]any{
			"Type":   "layers",
			"Layers": []string{"sha256:" + strings.Repeat("a", 64)},
		},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal docker inspect: %v", err)
	}
	return string(b)
}

// wantContentDigestFromDocker computes the content digest a docker-inspect body
// canonicalizes to, the value the composer/resolver must return for it.
func wantContentDigestFromDocker(t *testing.T, inspectJSON string) string {
	t.Helper()
	cc, err := imgconfig.FromDockerInspect([]byte(inspectJSON))
	if err != nil {
		t.Fatalf("FromDockerInspect: %v", err)
	}
	d, err := cc.ContentDigest()
	if err != nil {
		t.Fatalf("ContentDigest: %v", err)
	}
	return d
}

const fakeFreezeDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var (
	errTestPull    = errors.New("pull failed")
	errTestInspect = errors.New("inspect failed")
)

// stubFreezeResolvers points the CLI's digest resolver + feature composer at
// fakes that return a fixed digest, so freeze runs end-to-end without Docker.
func stubFreezeResolvers(t *testing.T, digest string) {
	t.Helper()
	origDR, origFC := newDigestResolver, newFeatureComposer
	newDigestResolver = func() freeze.DigestResolver {
		return freeze.DigestResolverFunc(func(context.Context, string) (string, error) { return digest, nil })
	}
	newFeatureComposer = func() freeze.FeatureComposer {
		return freeze.FeatureComposerFunc(func(context.Context, string, []string) ([]freeze.PlatformDigest, error) {
			return bothArchDigests(digest), nil
		})
	}
	t.Cleanup(func() { newDigestResolver, newFeatureComposer = origDR, origFC })
}

// bothArchDigests wraps a single digest as a linux/amd64 + linux/arm64 per-arch
// set, so a stubbed composer produces a pin whose host-platform entry exists on
// either architecture the test runner uses.
func bothArchDigests(digest string) []freeze.PlatformDigest {
	return []freeze.PlatformDigest{
		{OS: "linux", Arch: "amd64", Digest: digest},
		{OS: "linux", Arch: "arm64", Digest: digest},
	}
}

// writeSigningKey writes a fresh PKCS#8 ed25519 PEM key to a temp file and
// returns its path.
func writeSigningKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// installExample copies the worked example into the temp store under its
// name so `skill freeze <name>` can read it.
func installExample(t *testing.T, storeDir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForTest(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(storeDir, "weekly-metrics-digest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunSkillFreeze_HappyPath(t *testing.T) {
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Froze skill \"weekly-metrics-digest\"") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(out, "ContentHash: sha256:") {
		t.Errorf("stdout missing content hash: %q", out)
	}

	// A frozen version was written with a verifiable signature.
	s := store.New(storeDir)
	ids, err := s.FrozenVersions("weekly-metrics-digest")
	if err != nil || len(ids) != 1 {
		t.Fatalf("FrozenVersions = %v, %v", ids, err)
	}
	v, err := s.ReadFrozen("weekly-metrics-digest", ids[0])
	if err != nil {
		t.Fatalf("ReadFrozen: %v", err)
	}
	if len(v.Signature) == 0 || len(v.PublicKey) == 0 {
		t.Error("frozen version missing signature/public key")
	}
	// The lockfile pins the digest, never a tag.
	if !strings.Contains(string(v.Lockfile), fakeFreezeDigest) {
		t.Errorf("lockfile must pin the digest:\n%s", v.Lockfile)
	}
	if !strings.Contains(string(v.SkillMD), "lock:") {
		t.Errorf("frozen SKILL.md must carry the lock block:\n%s", v.SkillMD)
	}
	// The private key is never stored.
	if _, err := os.Stat(filepath.Join(s.FrozenDir("weekly-metrics-digest", ids[0]), "signing-key.pem")); !os.IsNotExist(err) {
		t.Error("the signing private key must never be written to the store")
	}
}

// TestRunSkillFreeze_MultiArchRecordsBothArchesInLock proves the S3 producer end
// to end through the CLI: a full freeze of a tools-declaring skill runs the REAL
// builderFeatureComposer (buildx preflight + multi-arch build + per-arch layout
// read) and records BOTH linux/amd64 and linux/arm64 config digests in the signed
// lock (#2036). The base digest resolver is stubbed and the OCI-layout read seam
// returns a fixed two-arch set, so the run needs no Docker or emulation.
func TestRunSkillFreeze_MultiArchRecordsBothArchesInLock(t *testing.T) {
	// host-npx avoids the managed provisioner's network fetch; the fake runner
	// answers the buildx build with a no-op.
	t.Setenv(container.ToolchainModeEnv, container.ToolchainModeHostNPX)
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	key := writeSigningKey(t)

	// Stub the base digest resolver; keep the REAL composer wired.
	origDR := newDigestResolver
	newDigestResolver = func() freeze.DigestResolver {
		return freeze.DigestResolverFunc(func(context.Context, string) (string, error) {
			return "sha256:" + strings.Repeat("e", 64), nil
		})
	}
	t.Cleanup(func() { newDigestResolver = origDR })

	amd := "sha256:" + strings.Repeat("a", 64)
	arm := "sha256:" + strings.Repeat("b", 64)
	stubOCILayoutDigests(t, []ociremote.PlatformConfigDigest{
		{OS: "linux", Arch: "amd64", Digest: amd},
		{OS: "linux", Arch: "arm64", Digest: arm},
	})
	fr := &fakeRunner{outputs: buildxPreflightOK()}
	withFakeInspector(t, fr)

	var stdout, stderr bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "weekly-metrics-digest"}, &stdout, &stderr); code != 0 {
		t.Fatalf("multi-arch freeze must succeed, exit=%d stderr=%s", code, stderr.String())
	}

	s := store.New(storeDir)
	ids, err := s.FrozenVersions("weekly-metrics-digest")
	if err != nil || len(ids) != 1 {
		t.Fatalf("FrozenVersions = %v, %v", ids, err)
	}
	v, err := s.ReadFrozen("weekly-metrics-digest", ids[0])
	if err != nil {
		t.Fatalf("ReadFrozen: %v", err)
	}
	lf := string(v.Lockfile)
	for _, want := range []string{"arch: amd64", "arch: arm64", amd, arm} {
		if !strings.Contains(lf, want) {
			t.Errorf("lockfile must record %q for the multi-arch pin:\n%s", want, lf)
		}
	}
}

// TestRunSkillFreeze_MissingBuildxAbortsWithNoPin proves the freeze aborts with the
// buildx/binfmt remediation and writes NO frozen version when the multi-arch
// preflight fails (#2036, Q2): a partial single-arch pin is never emitted.
func TestRunSkillFreeze_MissingBuildxAbortsWithNoPin(t *testing.T) {
	t.Setenv(container.ToolchainModeEnv, container.ToolchainModeHostNPX)
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	key := writeSigningKey(t)

	origDR := newDigestResolver
	newDigestResolver = func() freeze.DigestResolver {
		return freeze.DigestResolverFunc(func(context.Context, string) (string, error) {
			return "sha256:" + strings.Repeat("e", 64), nil
		})
	}
	t.Cleanup(func() { newDigestResolver = origDR })

	fr := &fakeRunner{fails: map[string]error{"buildx version": errors.New("unknown command: buildx")}}
	withFakeInspector(t, fr)

	var stdout, stderr bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "weekly-metrics-digest"}, &stdout, &stderr); code == 0 {
		t.Fatalf("a missing-buildx freeze must exit non-zero, stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "tonistiigi/binfmt") {
		t.Errorf("stderr = %q, want the buildx/binfmt remediation", stderr.String())
	}
	s := store.New(storeDir)
	if ids, _ := s.FrozenVersions("weekly-metrics-digest"); len(ids) != 0 {
		t.Errorf("a preflight-aborted freeze must write no frozen version, got %v", ids)
	}
}

func TestRunSkillFreeze_ReFreezeNewVersionKeepsPrior(t *testing.T) {
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var b1, e1 bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "weekly-metrics-digest"}, &b1, &e1); code != 0 {
		t.Fatalf("first freeze exit=%d stderr=%s", code, e1.String())
	}
	var b2, e2 bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "2.0.0", "weekly-metrics-digest"}, &b2, &e2); code != 0 {
		t.Fatalf("second freeze exit=%d stderr=%s", code, e2.String())
	}

	s := store.New(storeDir)
	ids, err := s.FrozenVersions("weekly-metrics-digest")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("re-freeze with a different version must create a new version dir, got %d", len(ids))
	}
}

func TestRunSkillFreeze_UnmarkedLLMFails(t *testing.T) {
	storeDir := withTempStore(t)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	// A skill whose step kind is unknown (a smuggling vector).
	const badMD = `---
name: bad-skill
description: Bad.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:x.y
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
  steps:
    - id: sneaky
      kind: not-a-kind
---

# Bad
`
	dir := filepath.Join(storeDir, "bad-skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(badMD), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "bad-skill"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("freeze of a skill with a bad step kind must exit non-zero")
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("expected an error on stderr, got: %s", stderr.String())
	}
}

func TestRunSkillFreeze_MissingSigningKey(t *testing.T) {
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	t.Setenv(freeze.SigningKeyEnv, "")

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("freeze with no signing key must exit non-zero")
	}
	if !strings.Contains(stderr.String(), freeze.SigningKeyEnv) {
		t.Errorf("error should mention the signing-key env, got: %s", stderr.String())
	}
}

// TestRunSkillFreeze_PublisherRecordedInLock proves `--publisher` is threaded
// into the frozen lock (#1900), so a launch can enforce publisher trust.
func TestRunSkillFreeze_PublisherRecordedInLock(t *testing.T) {
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "--publisher", "github://acme/plans", "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	// No omitted-publisher warning when --publisher is supplied.
	if strings.Contains(stderr.String(), "without --publisher") {
		t.Errorf("supplying --publisher must not warn: %q", stderr.String())
	}
	s := store.New(storeDir)
	ids, err := s.FrozenVersions("weekly-metrics-digest")
	if err != nil || len(ids) != 1 {
		t.Fatalf("FrozenVersions = %v, %v", ids, err)
	}
	v, err := s.ReadFrozen("weekly-metrics-digest", ids[0])
	if err != nil {
		t.Fatalf("ReadFrozen: %v", err)
	}
	if !strings.Contains(string(v.Lockfile), "publisher: github://acme/plans") {
		t.Errorf("lockfile must record the publisher:\n%s", v.Lockfile)
	}
	if !strings.Contains(string(v.SkillMD), "publisher: github://acme/plans") {
		t.Errorf("frozen manifest lock block must record the publisher:\n%s", v.SkillMD)
	}
}

// TestRunSkillFreeze_OmittedPublisherWarnsButSucceeds proves omitting
// --publisher prints a warning to stderr (never silently succeeds) and still
// freezes (#1900, P1).
func TestRunSkillFreeze_OmittedPublisherWarnsButSucceeds(t *testing.T) {
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("omitting --publisher must still freeze; exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "without --publisher") {
		t.Errorf("omitting --publisher must warn on stderr, got: %q", stderr.String())
	}
	s := store.New(storeDir)
	ids, _ := s.FrozenVersions("weekly-metrics-digest")
	v, err := s.ReadFrozen("weekly-metrics-digest", ids[0])
	if err != nil {
		t.Fatalf("ReadFrozen: %v", err)
	}
	if strings.Contains(string(v.Lockfile), "publisher:") {
		t.Errorf("a publisher-less freeze must not emit a publisher key:\n%s", v.Lockfile)
	}
}

// TestRunSkillFreeze_BareOwnerPublisherAccepted proves a bare-owner publisher
// authority (github://owner, which ParseFQN alone rejects) is accepted and
// sealed, matching the input forms `aileron keyring trust` accepts.
func TestRunSkillFreeze_BareOwnerPublisherAccepted(t *testing.T) {
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "--publisher", "github://acme", "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("a bare-owner --publisher must be accepted; exit=%d stderr=%s", code, stderr.String())
	}
	s := store.New(storeDir)
	ids, _ := s.FrozenVersions("weekly-metrics-digest")
	v, err := s.ReadFrozen("weekly-metrics-digest", ids[0])
	if err != nil {
		t.Fatalf("ReadFrozen: %v", err)
	}
	if !strings.Contains(string(v.Lockfile), "publisher: github://acme") {
		t.Errorf("lockfile must record the bare-owner publisher:\n%s", v.Lockfile)
	}
}

// TestRunSkillFreeze_InvalidPublisherFails proves a malformed --publisher fails
// fast before signing rather than sealing an unusable authority.
func TestRunSkillFreeze_InvalidPublisherFails(t *testing.T) {
	storeDir := withTempStore(t)
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "--publisher", "not-a-valid-authority", "weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("a malformed --publisher must exit non-zero")
	}
	if !strings.Contains(stderr.String(), "invalid --publisher") {
		t.Errorf("stderr = %q, want an invalid-publisher message", stderr.String())
	}
}

func TestRunSkillFreeze_InstructionStyleNoExecEnvStillSigns(t *testing.T) {
	storeDir := withTempStore(t)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	const noEnvMD = `---
name: no-env
description: No execution environment.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:x.y
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

# No Env
`
	dir := filepath.Join(storeDir, "no-env")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(noEnvMD), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "no-env"}, &stdout, &stderr); code != 0 {
		t.Fatalf("no-exec-env freeze must succeed, exit=%d stderr=%s", code, stderr.String())
	}
	s := store.New(storeDir)
	ids, err := s.FrozenVersions("no-env")
	if err != nil || len(ids) != 1 {
		t.Fatalf("FrozenVersions = %v, %v", ids, err)
	}
	v, err := s.ReadFrozen("no-env", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Signature) == 0 {
		t.Error("no-exec-env skill must still be signed")
	}
}

// TestRunSkillFreeze_EnvironmentImagePinsDigest proves the CLI freeze path
// for the environment.image escape hatch: the declared custom base resolves
// through the CLI's digest-resolver seam to a digest pin recorded in the
// lockfile, and the result is signed.
func TestRunSkillFreeze_EnvironmentImagePinsDigest(t *testing.T) {
	storeDir := withTempStore(t)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	const envImageMD = `---
name: env-image-skill
description: A skill naming a custom base image.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:x.y
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
    image: registry.example.com/base:1.4
  inputs: []
  outputs: []
---

# Env Image
`
	dir := filepath.Join(storeDir, "env-image-skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(envImageMD), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "env-image-skill"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("an environment-image freeze must succeed, exit=%d stderr=%s", code, stderr.String())
	}

	s := store.New(storeDir)
	ids, err := s.FrozenVersions("env-image-skill")
	if err != nil || len(ids) != 1 {
		t.Fatalf("FrozenVersions = %v, %v", ids, err)
	}
	v, err := s.ReadFrozen("env-image-skill", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(v.Lockfile), "registry.example.com/base:1.4") {
		t.Errorf("an environment-image freeze must pin the declared base ref, lockfile:\n%s", v.Lockfile)
	}
	if !strings.Contains(string(v.Lockfile), fakeFreezeDigest) {
		t.Errorf("an environment-image freeze must pin the resolved digest, lockfile:\n%s", v.Lockfile)
	}
	if len(v.Signature) == 0 {
		t.Error("an environment-image freeze must still be signed")
	}
}

// TestRunSkillFreeze_ToolsComposeOntoCustomBase proves the CLI freeze path
// for the composed environment: a skill declaring a custom base image AND
// curated tools freezes by digest-resolving the base, then handing the
// composer the DIGEST-PINNED base plus the CATALOG-RESOLVED Feature
// references. The lockfile pins the composed digest and records the declared
// tools as the capability set.
func TestRunSkillFreeze_ToolsComposeOntoCustomBase(t *testing.T) {
	storeDir := withTempStore(t)
	key := writeSigningKey(t)

	baseDigest := "sha256:" + strings.Repeat("a", 64)
	composedDigest := "sha256:" + strings.Repeat("b", 64)
	origDR, origFC := newDigestResolver, newFeatureComposer
	var resolvedRef, composeBase string
	var composeFeatures []string
	newDigestResolver = func() freeze.DigestResolver {
		return freeze.DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
			resolvedRef = ref
			return baseDigest, nil
		})
	}
	newFeatureComposer = func() freeze.FeatureComposer {
		return freeze.FeatureComposerFunc(func(_ context.Context, base string, features []string) ([]freeze.PlatformDigest, error) {
			composeBase = base
			composeFeatures = features
			return bothArchDigests(composedDigest), nil
		})
	}
	t.Cleanup(func() { newDigestResolver, newFeatureComposer = origDR, origFC })

	const toolsMD = `---
name: tools-on-base
description: Curated tools onto a custom base.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:x.y
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
    image: registry.example.com/base:1.4
    tools:
      - aws-cli@2.x
  inputs: []
  outputs: []
---

# Tools On Base
`
	dir := filepath.Join(storeDir, "tools-on-base")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(toolsMD), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runSkillFreeze([]string{"--signing-key", key, "tools-on-base"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("a tools+custom-base freeze must succeed, exit=%d stderr=%s", code, stderr.String())
	}
	if resolvedRef != "registry.example.com/base:1.4" {
		t.Errorf("resolver got %q, want the declared custom base", resolvedRef)
	}
	if want := "registry.example.com/base:1.4@" + baseDigest; composeBase != want {
		t.Errorf("composer got base %q, want the digest-pinned base %q", composeBase, want)
	}
	if want := composition.FeatureReference("aws-cli"); strings.Join(composeFeatures, ",") != want {
		t.Errorf("composer got features %v, want the catalog-resolved %q", composeFeatures, want)
	}

	s := store.New(storeDir)
	ids, err := s.FrozenVersions("tools-on-base")
	if err != nil || len(ids) != 1 {
		t.Fatalf("FrozenVersions = %v, %v", ids, err)
	}
	v, err := s.ReadFrozen("tools-on-base", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(v.Lockfile), composedDigest) {
		t.Errorf("the lockfile must pin the composed digest:\n%s", v.Lockfile)
	}
	if !strings.Contains(string(v.Lockfile), "aws-cli@2.x") {
		t.Errorf("the lockfile must record the declared tools:\n%s", v.Lockfile)
	}
	// The composed pin records the bootable local-daemon tag (#1856), computed
	// from the same digest-pinned base and catalog-resolved feature refs the
	// composer received, so the runtime boots the composed image.
	wantTag := composition.LocalToolsImageTag(composeBase, composeFeatures)
	if !strings.Contains(string(v.Lockfile), "localTag: "+wantTag) {
		t.Errorf("the lockfile must record the composed pin's bootable localTag %q:\n%s", wantTag, v.Lockfile)
	}
}

func TestRunSkillFreeze_FromPath(t *testing.T) {
	withTempStore(t)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)
	src := exampleSource(t) // a directory containing the worked example SKILL.md

	var stdout, stderr bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, src}, &stdout, &stderr); code != 0 {
		t.Fatalf("freeze from a path must succeed, exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Froze skill \"weekly-metrics-digest\"") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunSkillFreeze_UnknownTarget(t *testing.T) {
	withTempStore(t)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var stdout, stderr bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "does-not-exist"}, &stdout, &stderr); code == 0 {
		t.Error("freeze of an unknown target must exit non-zero")
	}
}

func TestRunSkillFreeze_RequiresExactlyOneTarget(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	if code := runSkillFreeze(nil, &stdout, &stderr); code != 1 {
		t.Errorf("no target exit = %d, want 1", code)
	}
	if code := runSkillFreeze([]string{"a", "b"}, &stdout, &stderr); code != 1 {
		t.Errorf("two targets exit = %d, want 1", code)
	}
}

func TestRunSkillFreeze_BadFlag(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	if code := runSkillFreeze([]string{"--nope"}, &stdout, &stderr); code != 1 {
		t.Errorf("bad flag exit = %d, want 1", code)
	}
}

func TestFrozenVersionID(t *testing.T) {
	id := frozenVersionID("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if id != "0123456789abcdef" {
		t.Errorf("frozenVersionID = %q", id)
	}
	if got := frozenVersionID("short"); got != "short" {
		t.Errorf("short hash = %q", got)
	}
}

func TestDigestFromRepoDigests(t *testing.T) {
	in := []byte(`["registry.example.com/runner@sha256:` + strings.Repeat("a", 64) + `"]`)
	got, err := digestFromRepoDigests(in, "registry.example.com/runner:1")
	if err != nil {
		t.Fatalf("digestFromRepoDigests: %v", err)
	}
	if got != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("digest = %q", got)
	}

	// Empty RepoDigests is a clear error (never silently pins nothing).
	if _, err := digestFromRepoDigests([]byte(`[]`), "x"); err == nil {
		t.Error("empty RepoDigests must error")
	}
	// Malformed JSON errors.
	if _, err := digestFromRepoDigests([]byte(`not json`), "x"); err == nil {
		t.Error("malformed RepoDigests must error")
	}
}

func TestDigestFromRepoDigests_MatchesRequestedRepo(t *testing.T) {
	want := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	// Two repositories for the same image; the requested repo's digest wins.
	in := []byte(`["mirror.example.com/runner@` + other + `","registry.example.com/runner@` + want + `"]`)
	got, err := digestFromRepoDigests(in, "registry.example.com/runner:1.4")
	if err != nil {
		t.Fatalf("digestFromRepoDigests: %v", err)
	}
	if got != want {
		t.Errorf("digest = %q, want the requested repo's digest %q", got, want)
	}

	// Multiple entries, none matching the requested repo: ambiguous, error
	// rather than pinning the wrong repository.
	if _, err := digestFromRepoDigests(in, "unrelated.example.com/thing:1"); err == nil {
		t.Error("multiple non-matching RepoDigests must error rather than guess")
	}
}

// fakeRunner is a container.Runner whose Run is scripted per command verb,
// so the inspector's pull-then-inspect and RepoDigests-then-Id fallback are
// exercised without Docker.
type fakeRunner struct {
	// outputs maps a join of the args to the stdout the command writes.
	outputs map[string]string
	// fails maps a join of the args to an error the command returns.
	fails map[string]error
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, args []string, stdout, _ io.Writer) error {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err, ok := f.fails[key]; ok {
		return err
	}
	if out, ok := f.outputs[key]; ok {
		_, _ = stdout.Write([]byte(out))
	}
	return nil
}

// withFakeInspector points newImageInspector at an inspector over the given
// fake runner, so the production ResolveDigest/ComposeDigest wrappers run
// their delegation without Docker.
func withFakeInspector(t *testing.T, fr *fakeRunner) {
	t.Helper()
	orig := newImageInspector
	newImageInspector = func() (imageInspector, error) {
		return imageInspector{runner: fr, runtime: "docker"}, nil
	}
	t.Cleanup(func() { newImageInspector = orig })
}

func TestRuntimeDigestResolver_DelegatesThroughInspector(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	fr := &fakeRunner{outputs: map[string]string{
		`image inspect --format {{json .RepoDigests}} img:1`: `["img@` + digest + `"]`,
	}}
	withFakeInspector(t, fr)
	got, err := runtimeDigestResolver{}.ResolveDigest(context.Background(), "img:1")
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}
	if got != digest {
		t.Errorf("digest = %q", got)
	}
}

func TestRuntimeDigestResolver_InspectorError(t *testing.T) {
	orig := newImageInspector
	newImageInspector = func() (imageInspector, error) {
		return imageInspector{}, errTestInspect
	}
	t.Cleanup(func() { newImageInspector = orig })
	if _, err := (runtimeDigestResolver{}).ResolveDigest(context.Background(), "img:1"); err == nil {
		t.Error("an inspector-construction error must surface")
	}
}

// TestBuilderFeatureComposer_LayoutReadErrorPropagates proves a failure reading
// the per-arch config digests back from the built OCI layout surfaces as an error
// (freeze must not seal a pin it could not attest).
func TestBuilderFeatureComposer_LayoutReadErrorPropagates(t *testing.T) {
	t.Setenv(container.ToolchainModeEnv, container.ToolchainModeHostNPX)
	origDir, origRead := freezeOCILayoutDir, readOCILayoutConfigDigests
	freezeOCILayoutDir = func(string) (string, error) { return t.TempDir(), nil }
	readOCILayoutConfigDigests = func(context.Context, string) ([]ociremote.PlatformConfigDigest, error) {
		return nil, errors.New("layout corrupt")
	}
	t.Cleanup(func() { freezeOCILayoutDir, readOCILayoutConfigDigests = origDir, origRead })

	fr := &fakeRunner{outputs: buildxPreflightOK()}
	withFakeInspector(t, fr)
	if _, err := (builderFeatureComposer{}).ComposeDigest(context.Background(), "base@"+fakeFreezeDigest, []string{"aws-cli"}); err == nil || !strings.Contains(err.Error(), "per-arch config digests") {
		t.Fatalf("err = %v, want the layout-read failure", err)
	}
}

// TestBuilderFeatureComposer_LayoutDirPrepareErrors proves a non-creatable layout
// directory (its parent is a regular file) fails fast before any build runs.
func TestBuilderFeatureComposer_LayoutDirPrepareErrors(t *testing.T) {
	t.Setenv(container.ToolchainModeEnv, container.ToolchainModeHostNPX)
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	origDir := freezeOCILayoutDir
	freezeOCILayoutDir = func(string) (string, error) { return filepath.Join(parent, "layout"), nil }
	t.Cleanup(func() { freezeOCILayoutDir = origDir })

	fr := &fakeRunner{outputs: buildxPreflightOK()}
	withFakeInspector(t, fr)
	if _, err := (builderFeatureComposer{}).ComposeDigest(context.Background(), "base@"+fakeFreezeDigest, []string{"aws-cli"}); err == nil || !strings.Contains(err.Error(), "OCI layout dir") {
		t.Fatalf("err = %v, want a layout-dir prepare error", err)
	}
}

func TestBuilderFeatureComposer_InspectorError(t *testing.T) {
	orig := newImageInspector
	newImageInspector = func() (imageInspector, error) {
		return imageInspector{}, errTestInspect
	}
	t.Cleanup(func() { newImageInspector = orig })
	if _, err := (builderFeatureComposer{}).ComposeDigest(context.Background(), "base@"+fakeFreezeDigest, []string{"f"}); err == nil {
		t.Error("an inspector-construction error must surface from ComposeDigest")
	}
}

// buildxPreflightOK is the fake-runner output that makes CheckMultiArchBuild pass:
// `buildx version` succeeds (empty output, no error) and `buildx inspect
// --bootstrap` advertises both required platforms.
func buildxPreflightOK() map[string]string {
	return map[string]string{
		"buildx inspect --bootstrap": "Platforms: linux/amd64, linux/arm64, linux/arm/v7\n",
	}
}

// stubOCILayoutDigests points the composer's OCI-layout reader at a fixed
// per-arch set and redirects the layout directory to a temp dir, so the composer
// runs its preflight + build orchestration + mapping without a real buildx build
// or an on-disk layout (the layout read is covered in the ociremote package).
func stubOCILayoutDigests(t *testing.T, digests []ociremote.PlatformConfigDigest) {
	t.Helper()
	origDir, origRead := freezeOCILayoutDir, readOCILayoutConfigDigests
	freezeOCILayoutDir = func(string) (string, error) { return t.TempDir(), nil }
	readOCILayoutConfigDigests = func(context.Context, string) ([]ociremote.PlatformConfigDigest, error) {
		return digests, nil
	}
	t.Cleanup(func() { freezeOCILayoutDir, readOCILayoutConfigDigests = origDir, origRead })
}

// TestBuilderFeatureComposer_ResolvesHostNPXToolchain is the regression guard
// for #1866: the composer must resolve the toolchain from the environment and
// wire it into the Builder. With AILERON_SANDBOX_TOOLCHAIN=host-npx set, the
// composer resolves the host-npx opt-out (no provisioner) and composes the
// multi-arch image successfully, returning the per-arch config-digest set.
func TestBuilderFeatureComposer_ResolvesHostNPXToolchain(t *testing.T) {
	t.Setenv(container.ToolchainModeEnv, container.ToolchainModeHostNPX)

	amd := "sha256:" + strings.Repeat("a", 64)
	arm := "sha256:" + strings.Repeat("b", 64)
	stubOCILayoutDigests(t, []ociremote.PlatformConfigDigest{
		{OS: "linux", Arch: "amd64", Digest: amd},
		{OS: "linux", Arch: "arm64", Digest: arm},
	})
	fr := &fakeRunner{outputs: buildxPreflightOK()}
	withFakeInspector(t, fr)

	got, err := (builderFeatureComposer{}).ComposeDigest(context.Background(), "base@"+fakeFreezeDigest, []string{"aws-cli"})
	if err != nil {
		t.Fatalf("ComposeDigest with host-npx toolchain: %v", err)
	}
	assertTwoArchDigests(t, got, amd, arm)
}

// TestBuilderFeatureComposer_ManagedToolchainWiresProvisioner covers the managed
// branch of the #1866 fix: when the resolved toolchain is managed (the default),
// ComposeDigest wires the managed provisioner onto the Builder. The managed
// escape-hatch env points at on-disk paths so the managed branch does not attempt
// a network provision. The composer resolves managed, takes the escape hatch, and
// returns the per-arch set.
func TestBuilderFeatureComposer_ManagedToolchainWiresProvisioner(t *testing.T) {
	t.Setenv(container.ToolchainModeEnv, "")
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	cli := filepath.Join(dir, "devcontainer.js")
	if err := os.WriteFile(node, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake node: %v", err)
	}
	if err := os.WriteFile(cli, []byte("// cli\n"), 0o644); err != nil {
		t.Fatalf("write fake cli entrypoint: %v", err)
	}
	t.Setenv(container.NodeBinaryEnv, node)
	t.Setenv(container.DevcontainerCLIEnv, cli)

	amd := "sha256:" + strings.Repeat("c", 64)
	arm := "sha256:" + strings.Repeat("d", 64)
	stubOCILayoutDigests(t, []ociremote.PlatformConfigDigest{
		{OS: "linux", Arch: "amd64", Digest: amd},
		{OS: "linux", Arch: "arm64", Digest: arm},
	})
	fr := &fakeRunner{outputs: buildxPreflightOK()}
	withFakeInspector(t, fr)

	got, err := (builderFeatureComposer{}).ComposeDigest(context.Background(), "base@"+fakeFreezeDigest, []string{"aws-cli"})
	if err != nil {
		t.Fatalf("ComposeDigest with managed toolchain + escape hatch: %v", err)
	}
	assertTwoArchDigests(t, got, amd, arm)
}

func assertTwoArchDigests(t *testing.T, got []freeze.PlatformDigest, amd, arm string) {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("want two per-arch digests, got %+v", got)
	}
	byArch := map[string]string{}
	for _, p := range got {
		byArch[p.Arch] = p.Digest
	}
	if byArch["amd64"] != amd {
		t.Errorf("amd64 digest = %q, want %q", byArch["amd64"], amd)
	}
	if byArch["arm64"] != arm {
		t.Errorf("arm64 digest = %q, want %q", byArch["arm64"], arm)
	}
}

// TestBuilderFeatureComposer_MissingBuildxReturnsRemediation proves the multi-arch
// preflight fails closed: when `docker buildx version` is unavailable, the composer
// returns the actionable tonistiigi/binfmt remediation and never reads a layout or
// emits a pin (#2036, Q2).
func TestBuilderFeatureComposer_MissingBuildxReturnsRemediation(t *testing.T) {
	readHit := false
	origDir, origRead := freezeOCILayoutDir, readOCILayoutConfigDigests
	freezeOCILayoutDir = func(string) (string, error) { return t.TempDir(), nil }
	readOCILayoutConfigDigests = func(context.Context, string) ([]ociremote.PlatformConfigDigest, error) {
		readHit = true
		return nil, nil
	}
	t.Cleanup(func() { freezeOCILayoutDir, readOCILayoutConfigDigests = origDir, origRead })

	fr := &fakeRunner{fails: map[string]error{"buildx version": errors.New("unknown command: buildx")}}
	withFakeInspector(t, fr)

	_, err := (builderFeatureComposer{}).ComposeDigest(context.Background(), "base@"+fakeFreezeDigest, []string{"aws-cli"})
	if err == nil || !strings.Contains(err.Error(), "tonistiigi/binfmt") {
		t.Fatalf("err = %v, want a buildx/binfmt remediation", err)
	}
	if readHit {
		t.Error("a failed preflight must not read the OCI layout")
	}
}

func TestImageInspector_ResolveDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	fr := &fakeRunner{outputs: map[string]string{
		`image inspect --format {{json .RepoDigests}} registry.example.com/runner:1.4`: `["registry.example.com/runner@` + digest + `"]`,
	}}
	in := imageInspector{runner: fr, runtime: "docker"}
	got, err := in.resolveDigest(context.Background(), "registry.example.com/runner:1.4")
	if err != nil {
		t.Fatalf("resolveDigest: %v", err)
	}
	if got != digest {
		t.Errorf("digest = %q, want %q", got, digest)
	}
	// A pull was attempted (best-effort) before the inspect.
	if len(fr.calls) == 0 || !strings.HasPrefix(fr.calls[0], "pull ") {
		t.Errorf("expected a best-effort pull first, calls=%v", fr.calls)
	}
}

func TestImageInspector_ResolveDigest_PullFailsButLocalInspectSucceeds(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	fr := &fakeRunner{
		fails: map[string]error{"pull img:1": errTestPull},
		outputs: map[string]string{
			`image inspect --format {{json .RepoDigests}} img:1`: `["img@` + digest + `"]`,
		},
	}
	in := imageInspector{runner: fr, runtime: "docker"}
	got, err := in.resolveDigest(context.Background(), "img:1")
	if err != nil {
		t.Fatalf("a failed pull must not abort when the image is local: %v", err)
	}
	if got != digest {
		t.Errorf("digest = %q", got)
	}
}

func TestImageInspector_ResolveDigest_InspectFails(t *testing.T) {
	fr := &fakeRunner{fails: map[string]error{
		`image inspect --format {{json .RepoDigests}} img:1`: errTestInspect,
	}}
	in := imageInspector{runner: fr, runtime: "docker"}
	if _, err := in.resolveDigest(context.Background(), "img:1"); err == nil {
		t.Error("a failing inspect must error")
	}
}

func TestImageInspector_LocalImageContentDigest_CanonicalizesInspect(t *testing.T) {
	inspect := dockerInspectJSON(t, "built")
	want := wantContentDigestFromDocker(t, inspect)
	fr := &fakeRunner{outputs: map[string]string{
		`image inspect --format {{json .}} built:local`: inspect + "\n",
	}}
	in := imageInspector{runner: fr, runtime: "docker"}
	got, err := in.localImageContentDigest(context.Background(), "built:local")
	if err != nil {
		t.Fatalf("localImageContentDigest: %v", err)
	}
	if got != want {
		t.Errorf("digest = %q, want the config content digest %q", got, want)
	}
}

func TestImageInspector_LocalImageContentDigest_InspectFails(t *testing.T) {
	fr := &fakeRunner{fails: map[string]error{
		`image inspect --format {{json .}} built:local`: errTestInspect,
	}}
	in := imageInspector{runner: fr, runtime: "docker"}
	if _, err := in.localImageContentDigest(context.Background(), "built:local"); err == nil {
		t.Error("a failing inspect must error")
	}
}

func TestImageInspector_LocalImageContentDigest_InvalidJSONRejected(t *testing.T) {
	fr := &fakeRunner{outputs: map[string]string{
		`image inspect --format {{json .}} built:local`: "not json\n",
	}}
	in := imageInspector{runner: fr, runtime: "docker"}
	if _, err := in.localImageContentDigest(context.Background(), "built:local"); err == nil {
		t.Error("an unparsable inspect body must be rejected")
	}
}

func TestReadSkillForFreeze_NonSkillFilePath(t *testing.T) {
	withTempStore(t)
	dir := t.TempDir()
	notSkill := filepath.Join(dir, "README.md")
	if err := os.WriteFile(notSkill, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSkillForFreeze(notSkill); err == nil {
		t.Error("a non-SKILL.md file path must error")
	}
}

func TestReadSkillForFreeze_DirectoryWithoutSkill(t *testing.T) {
	withTempStore(t)
	if _, err := readSkillForFreeze(t.TempDir()); err == nil {
		t.Error("a directory with no SKILL.md must error")
	}
}

func TestRepoOfRef(t *testing.T) {
	cases := map[string]string{
		"registry.example.com/runner:1.4":             "registry.example.com/runner",
		"registry.example.com:5000/runner:1.4":        "registry.example.com:5000/runner",
		"registry.example.com/runner@sha256:" + "abc": "registry.example.com/runner",
		"runner":                                "runner",
		"registry.example.com:5000/team/runner": "registry.example.com:5000/team/runner",
	}
	for in, want := range cases {
		if got := repoOfRef(in); got != want {
			t.Errorf("repoOfRef(%q) = %q, want %q", in, got, want)
		}
	}
}
