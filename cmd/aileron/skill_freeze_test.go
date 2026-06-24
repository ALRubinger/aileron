package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

const fakeFreezeDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// stubFreezeResolvers points the CLI's digest resolver + feature composer at
// fakes that return a fixed digest, so freeze runs end-to-end without Docker.
func stubFreezeResolvers(t *testing.T, digest string) {
	t.Helper()
	origDR, origFC := newDigestResolver, newFeatureComposer
	newDigestResolver = func() freeze.DigestResolver {
		return freeze.DigestResolverFunc(func(context.Context, string) (string, error) { return digest, nil })
	}
	newFeatureComposer = func() freeze.FeatureComposer {
		return freeze.FeatureComposerFunc(func(context.Context, []string) (string, error) { return digest, nil })
	}
	t.Cleanup(func() { newDigestResolver, newFeatureComposer = origDR, origFC })
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
