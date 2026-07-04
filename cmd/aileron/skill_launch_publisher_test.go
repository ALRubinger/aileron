package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// genKeyPair returns a fresh ed25519 keypair plus the path to its PKCS#8 PEM
// private key, so a test controls both the signing key freeze uses and the
// public key it registers in a keyring.
func genKeyPair(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
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
	return pub, path
}

// writeKeyring builds a keyring trusting each (ownerAuthority -> keys) and
// (perRepoAuthority -> keys) entry and saves it at path.
func writeOwnerKeyring(t *testing.T, path, ownerAuthority string, keys ...ed25519.PublicKey) {
	t.Helper()
	ring := cstore.NewEd25519Keyring()
	for _, k := range keys {
		ring.AddOwner(ownerAuthority, k)
	}
	if err := ring.SaveKeyring(path); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
}

// TestKeyringPublisherVerifier_TrustedEmitsDiag proves a trusted publisher
// verifies and emits a trust line on the held diag (stderr) writer.
func TestKeyringPublisherVerifier_TrustedEmitsDiag(t *testing.T) {
	pub, _ := genKeyPair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	writeOwnerKeyring(t, path, "github://acme", pub)

	var diag bytes.Buffer
	v := keyringPublisherVerifier{path: path, diag: &diag}
	if err := v.VerifyPublisher("github://acme/plans", pub); err != nil {
		t.Fatalf("trusted publisher must verify: %v", err)
	}
	if !strings.Contains(diag.String(), "trusted") {
		t.Errorf("diag = %q, want a trust line", diag.String())
	}
	if strings.Contains(diag.String(), "disagree") {
		t.Errorf("a single owner grant must not emit a conflict note: %q", diag.String())
	}
}

// TestKeyringPublisherVerifier_UntrustedFailsClosed proves an untrusted key
// yields a fail-closed error naming the publisher.
func TestKeyringPublisherVerifier_UntrustedFailsClosed(t *testing.T) {
	trusted, _ := genKeyPair(t)
	planKey, _ := genKeyPair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	writeOwnerKeyring(t, path, "github://acme", trusted)

	v := keyringPublisherVerifier{path: path}
	err := v.VerifyPublisher("github://acme/plans", planKey)
	if err == nil {
		t.Fatal("an untrusted signing key must fail closed")
	}
	if !strings.Contains(err.Error(), "not trusted") || !strings.Contains(err.Error(), "github://acme/plans") {
		t.Errorf("error = %q, want a not-trusted message naming the publisher", err.Error())
	}
}

// TestKeyringPublisherVerifier_RevokedFailsClosed proves that after the
// trusted key is revoked (removed from the keyring on disk), a subsequent
// verify fails closed because the verifier re-reads the keyring each call.
func TestKeyringPublisherVerifier_RevokedFailsClosed(t *testing.T) {
	pub, _ := genKeyPair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	writeOwnerKeyring(t, path, "github://acme", pub)

	v := keyringPublisherVerifier{path: path}
	if err := v.VerifyPublisher("github://acme/plans", pub); err != nil {
		t.Fatalf("pre-revocation must verify: %v", err)
	}
	// Revoke by rewriting an empty keyring at the same path.
	if err := cstore.NewEd25519Keyring().SaveKeyring(path); err != nil {
		t.Fatalf("SaveKeyring empty: %v", err)
	}
	if err := v.VerifyPublisher("github://acme/plans", pub); err == nil {
		t.Error("after revocation the verifier must fail closed on the re-read keyring")
	}
}

// TestKeyringPublisherVerifier_ConflictNoteOnStderr proves the P2 conflict
// diagnostic lands on the held diag (stderr) writer when the owner and per-repo
// scopes disagree while membership passes, and the launch is still permitted.
func TestKeyringPublisherVerifier_ConflictNoteOnStderr(t *testing.T) {
	shared, _ := genKeyPair(t)
	ownerExtra, _ := genKeyPair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	ring := cstore.NewEd25519Keyring()
	ring.AddOwner("github://acme", shared)
	ring.AddOwner("github://acme", ownerExtra)
	ring.Add("github://acme/plans", shared)
	if err := ring.SaveKeyring(path); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}

	var diag bytes.Buffer
	v := keyringPublisherVerifier{path: path, diag: &diag}
	if err := v.VerifyPublisher("github://acme/plans", shared); err != nil {
		t.Fatalf("a conflict must not block a member key: %v", err)
	}
	if !strings.Contains(diag.String(), "disagree") {
		t.Errorf("diag = %q, want a conflict note", diag.String())
	}
}

// TestKeyringPublisherVerifier_MalformedKeyringSurfacesError proves a malformed
// keyring surfaces the parse error rather than a misleading not-trusted result.
func TestKeyringPublisherVerifier_MalformedKeyringSurfacesError(t *testing.T) {
	pub, _ := genKeyPair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := keyringPublisherVerifier{path: path}
	err := v.VerifyPublisher("github://acme/plans", pub)
	if err == nil {
		t.Fatal("a malformed keyring must surface an error")
	}
	if strings.Contains(err.Error(), "not trusted") {
		t.Errorf("a malformed keyring must not masquerade as not-trusted: %q", err.Error())
	}
}

// TestLaunchPublisherVerifier_NilOnImageBoot proves the CLI nils the verifier
// on the image-boot re-entry (mirrors the InPinnedImage guard), so the inner
// re-check never runs against the container's empty keyring.
func TestLaunchPublisherVerifier_NilOnImageBoot(t *testing.T) {
	t.Setenv(envSkillImageBooted, "1")
	if v := launchPublisherVerifier(&bytes.Buffer{}); v != nil {
		t.Errorf("image-boot re-entry must nil the publisher verifier, got %v", v)
	}
}

// TestLaunchPublisherVerifier_WiredOnHostLaunch proves a host launch (no boot
// sentinel) wires the keyring-backed verifier.
func TestLaunchPublisherVerifier_WiredOnHostLaunch(t *testing.T) {
	t.Setenv(envSkillImageBooted, "")
	if v := launchPublisherVerifier(&bytes.Buffer{}); v == nil {
		t.Error("a host launch must wire the publisher verifier")
	}
}

// freezeNoImageWithPublisher installs and freezes a no-image variant of the
// worked example attributed to publisher, signed with a caller-controlled key,
// and returns the public key so the test can register it in a keyring.
func freezeNoImageWithPublisher(t *testing.T, storeDir, publisher string) ed25519.PublicKey {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForTest(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	stripped := stripEnvironmentBlock(t, string(raw))
	dir := filepath.Join(storeDir, "weekly-metrics-digest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}
	stubFreezeResolvers(t, fakeFreezeDigest)
	pub, keyPath := genKeyPair(t)
	args := []string{"--signing-key", keyPath, "weekly-metrics-digest"}
	if publisher != "" {
		args = []string{"--signing-key", keyPath, "--publisher", publisher, "weekly-metrics-digest"}
	}
	var out, errOut bytes.Buffer
	if code := runSkillFreeze(args, &out, &errOut); code != 0 {
		t.Fatalf("freeze no-image with publisher failed: %s", errOut.String())
	}
	return pub
}

// stubLaunchPublisherVerifier points the launch's publisher-verifier seam at a
// verifier reading the keyring at path, so tests control trust without touching
// the host home directory (cross-platform: os.UserHomeDir reads %USERPROFILE%
// on Windows, not $HOME). The launch's boot-sentinel nil-skip still applies:
// launchPublisherVerifier only calls this seam on a host launch.
func stubLaunchPublisherVerifier(t *testing.T, path string) {
	t.Helper()
	orig := newLaunchPublisherVerifier
	newLaunchPublisherVerifier = func(diag io.Writer) runtime.PublisherVerifier {
		return keyringPublisherVerifier{path: path, diag: diag}
	}
	t.Cleanup(func() { newLaunchPublisherVerifier = orig })
}

// launchInProcessDispatcher returns the dispatcher wiring the no-image worked
// example needs to complete in-process.
func launchInProcessDispatcher() *fakeLaunchDispatcher {
	return &fakeLaunchDispatcher{results: map[string]map[string]any{
		"aileron:metrics.query_series": {
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8", "content": "{}",
		},
	}}
}

// TestRunSkillLaunch_TrustedPublisherLaunches proves the end-to-end host launch:
// a plan frozen with --publisher launches when the operator's keyring trusts the
// publisher for the plan's signing key.
func TestRunSkillLaunch_TrustedPublisherLaunches(t *testing.T) {
	storeDir := withTempStore(t)
	pub := freezeNoImageWithPublisher(t, storeDir, "github://acme/plans")

	keyringPath := filepath.Join(t.TempDir(), "keyring.json")
	writeOwnerKeyring(t, keyringPath, "github://acme", pub)
	stubLaunchPublisherVerifier(t, keyringPath)

	stubLaunchSeams(t, launchInProcessDispatcher())
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origSeam := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origSeam })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("a trusted-publisher launch must succeed; exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "trusted") {
		t.Errorf("stderr should carry the publisher-trust diagnostic: %q", stderr.String())
	}
}

// TestRunSkillLaunch_UntrustedPublisherFailsClosed proves a plan whose declared
// publisher is not trusted refuses to launch (P0 fail-closed).
func TestRunSkillLaunch_UntrustedPublisherFailsClosed(t *testing.T) {
	storeDir := withTempStore(t)
	freezeNoImageWithPublisher(t, storeDir, "github://acme/plans")

	// Point the verifier at a keyring path that does not exist: an empty keyring
	// trusts nothing, so the gate fails closed.
	stubLaunchPublisherVerifier(t, filepath.Join(t.TempDir(), "keyring.json"))

	stubLaunchSeams(t, launchInProcessDispatcher())
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origSeam := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origSeam })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("an untrusted-publisher launch must fail closed")
	}
	if !strings.Contains(stderr.String(), "not trusted") {
		t.Errorf("stderr = %q, want a not-trusted refusal", stderr.String())
	}
}

// TestRunSkillLaunch_PublisherlessLaunchesLocally proves a plan frozen WITHOUT
// a publisher still launches with no keyring present (P1: the gate applies only
// when the lock declares a publisher).
func TestRunSkillLaunch_PublisherlessLaunchesLocally(t *testing.T) {
	storeDir := withTempStore(t)
	freezeNoImageWithPublisher(t, storeDir, "") // no publisher declared

	// A verifier is wired, but the plan declares no publisher, so the runtime
	// must skip the gate and never read the (nonexistent) keyring.
	stubLaunchPublisherVerifier(t, filepath.Join(t.TempDir(), "keyring.json"))

	stubLaunchSeams(t, launchInProcessDispatcher())
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origSeam := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origSeam })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("a publisher-less plan must launch without a keyring; exit=%d stderr=%s", code, stderr.String())
	}
}

// TestRunSkillLaunch_ImageBootedPublisherPlanNoKeyring is the P0 regression:
// an image-pinned plan frozen with a publisher, re-entered inside the pin
// (AILERON_SKILL_IMAGE_BOOTED set) with NO host keyring present, must launch.
// The host already ran the gate before boot and no keyring is mounted into the
// sealed container; wiring the gate here would resolve an empty keyring and
// fail closed for every image-pinned plan. It must fail before the fix (verifier
// wired unconditionally) and pass after (verifier nil'd on the boot sentinel).
func TestRunSkillLaunch_ImageBootedPublisherPlanNoKeyring(t *testing.T) {
	storeDir := withTempStore(t)
	// Freeze the environment-declaring worked example (pins an image) WITH a
	// publisher and a controlled key.
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	_, keyPath := genKeyPair(t)
	var fout, ferr bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", keyPath, "--publisher", "github://acme/plans", "weekly-metrics-digest"}, &fout, &ferr); code != 0 {
		t.Fatalf("freeze image plan with publisher: %s", ferr.String())
	}

	// Simulate the in-container re-entry: the boot sentinel is set and no keyring
	// exists. InPinnedImage keeps the run in-process. The verifier seam points at
	// a nonexistent keyring: if the boot-sentinel nil-skip regressed and the gate
	// ran, that empty keyring would fail closed and this test would fail. With the
	// nil-skip intact the seam is never consulted and the launch proceeds.
	t.Setenv(envSkillImageBooted, "1")
	stubLaunchPublisherVerifier(t, filepath.Join(t.TempDir(), "keyring.json"))

	stubLaunchSeams(t, launchInProcessDispatcher())
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origSeam := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origSeam })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("an image-booted publisher plan with no keyring must launch (verifier nil'd on re-entry); exit=%d stderr=%s", code, stderr.String())
	}
}
