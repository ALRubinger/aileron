package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// TestKeyringTrustPlan_GrantsPerRepoSigningKey is the #2136 self-trust
// regression on the trust command itself: `keyring trust --plan <name>` reads
// the plan's OWN signing key (`signing-key.pub`) from the frozen store and
// grants it at the plan's declared per-repo publisher authority (lock.Publisher)
// with no network fetch. Before the flag existed there was no path to trust a
// plan whose signing key is not the repo's committed keys/publisher.pub.
func TestKeyringTrustPlan_GrantsPerRepoSigningKey(t *testing.T) {
	storeDir := withTempStore(t)
	withTempHome(t)
	pub := freezeNoImageWithPublisher(t, storeDir, "github://acme/plans")

	var out, errBuf bytes.Buffer
	if code := runKeyring([]string{"trust", "--plan", "weekly-metrics-digest"}, &out, &errBuf); code != 0 {
		t.Fatalf("keyring trust --plan must succeed; exit=?, stderr=%s", errBuf.String())
	}
	if !strings.Contains(out.String(), "github://acme/plans") {
		t.Errorf("stdout = %q, want the per-repo publisher authority", out.String())
	}

	// The grant must land at the per-repo authority (not owner) and hold exactly
	// the plan's signing key.
	kr, err := cstore.LoadKeyring(cstore.DefaultKeyringPath())
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	if !kr.HasKey("github://acme/plans", pub) {
		t.Errorf("plan's signing key not granted at per-repo authority github://acme/plans")
	}
	if kr.HasOwnerKey("github://acme", pub) {
		t.Errorf("--plan must NOT widen trust to the owner scope github://acme")
	}
}

// TestKeyringTrustPlan_PinnedVersion proves the `<name>@<version>` form trusts a
// specific frozen version's signing key by pinning its version id.
func TestKeyringTrustPlan_PinnedVersion(t *testing.T) {
	storeDir := withTempStore(t)
	withTempHome(t)
	pub := freezeNoImageWithPublisher(t, storeDir, "github://acme/plans")

	// Discover the frozen version id so the test pins it explicitly.
	ids, err := store.New(storeDir).FrozenVersions("weekly-metrics-digest")
	if err != nil || len(ids) != 1 {
		t.Fatalf("FrozenVersions = %v, err = %v; want exactly one id", ids, err)
	}

	var out, errBuf bytes.Buffer
	if code := runKeyring([]string{"trust", "--plan", "weekly-metrics-digest@" + ids[0]}, &out, &errBuf); code != 0 {
		t.Fatalf("pinned-version trust must succeed; stderr=%s", errBuf.String())
	}
	kr, err := cstore.LoadKeyring(cstore.DefaultKeyringPath())
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	if !kr.HasKey("github://acme/plans", pub) {
		t.Errorf("pinned-version trust did not grant the plan's signing key")
	}
}

// TestKeyringTrustPlan_Idempotent proves a second `keyring trust --plan` of the
// same plan is a no-op that reports the plan is already trusted rather than
// duplicating the key or erroring.
func TestKeyringTrustPlan_Idempotent(t *testing.T) {
	storeDir := withTempStore(t)
	withTempHome(t)
	freezeNoImageWithPublisher(t, storeDir, "github://acme/plans")

	var out, errBuf bytes.Buffer
	if code := runKeyring([]string{"trust", "--plan", "weekly-metrics-digest"}, &out, &errBuf); code != 0 {
		t.Fatalf("first trust must succeed; stderr=%s", errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := runKeyring([]string{"trust", "--plan", "weekly-metrics-digest"}, &out, &errBuf); code != 0 {
		t.Fatalf("second trust must succeed; stderr=%s", errBuf.String())
	}
	if !strings.Contains(out.String(), "already trusted") {
		t.Errorf("stdout = %q, want an already-trusted no-op on the second run", out.String())
	}

	kr, err := cstore.LoadKeyring(cstore.DefaultKeyringPath())
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	if got := len(kr.Keys("github://acme/plans")); got != 1 {
		t.Errorf("per-repo key count = %d, want 1 (no duplicate on re-run)", got)
	}
}

// TestKeyringTrustPlan_AlreadyCoveredByOwnerGrant proves that when an owner-level
// grant already covers the plan's signing key (launch's owner∪per-repo union is
// already satisfied), `--plan` is a no-op that names the covering owner scope and
// does not write a redundant per-repo grant.
func TestKeyringTrustPlan_AlreadyCoveredByOwnerGrant(t *testing.T) {
	storeDir := withTempStore(t)
	withTempHome(t)
	pub := freezeNoImageWithPublisher(t, storeDir, "github://acme/plans")

	// Seed an owner grant for the plan's own signing key, so the union already
	// trusts it before `--plan` runs.
	path := cstore.DefaultKeyringPath()
	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	kr.AddOwner("github://acme", pub)
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("save keyring: %v", err)
	}

	var out, errBuf bytes.Buffer
	if code := runKeyring([]string{"trust", "--plan", "weekly-metrics-digest"}, &out, &errBuf); code != 0 {
		t.Fatalf("trust must succeed as a no-op; stderr=%s", errBuf.String())
	}
	if !strings.Contains(out.String(), "already trusted via owner grant github://acme") {
		t.Errorf("stdout = %q, want the owner-scope covering note", out.String())
	}
	// No redundant per-repo grant should be written.
	kr2, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("reload keyring: %v", err)
	}
	if len(kr2.Keys("github://acme/plans")) != 0 {
		t.Errorf("--plan wrote a redundant per-repo grant despite owner-scope coverage")
	}
}

// TestKeyringTrustPlan_RejectsKeyFile proves --plan and --key-file are mutually
// exclusive: the plan's key source is the frozen store, never an external file.
func TestKeyringTrustPlan_RejectsKeyFile(t *testing.T) {
	withTempStore(t)
	withTempHome(t)
	var out, errBuf bytes.Buffer
	code := runKeyring([]string{"trust", "--plan", "demo", "--key-file", "/tmp/x.pub"}, &out, &errBuf)
	if code == 0 {
		t.Fatal("--plan with --key-file must be a usage error")
	}
	if !strings.Contains(errBuf.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want a mutual-exclusion error", errBuf.String())
	}
}

// TestKeyringTrustPlan_RejectsExtraAuthority proves --plan takes no positional
// authority argument (the authority comes from the plan's signed lock).
func TestKeyringTrustPlan_RejectsExtraAuthority(t *testing.T) {
	withTempStore(t)
	withTempHome(t)
	var out, errBuf bytes.Buffer
	code := runKeyring([]string{"trust", "--plan", "demo", "github://acme/plans"}, &out, &errBuf)
	if code == 0 {
		t.Fatal("--plan with a positional authority must be a usage error")
	}
	if !strings.Contains(errBuf.String(), "no authority argument") {
		t.Errorf("stderr = %q, want a no-authority-argument error", errBuf.String())
	}
}

// TestKeyringTrustPlan_NoPublisherErrors proves a plan frozen WITHOUT a
// publisher has no launch-time trust gate, so `--plan` reports there is nothing
// to trust rather than granting an empty authority.
func TestKeyringTrustPlan_NoPublisherErrors(t *testing.T) {
	storeDir := withTempStore(t)
	withTempHome(t)
	freezeNoImageWithPublisher(t, storeDir, "") // no publisher declared

	var out, errBuf bytes.Buffer
	code := runKeyring([]string{"trust", "--plan", "weekly-metrics-digest"}, &out, &errBuf)
	if code == 0 {
		t.Fatal("a publisher-less plan has no trust gate; --plan must error")
	}
	if !strings.Contains(errBuf.String(), "no publisher") {
		t.Errorf("stderr = %q, want a no-publisher error", errBuf.String())
	}
}

// TestKeyringTrustPlan_UnknownPlanErrors proves --plan for a name with no frozen
// versions errors with the freeze-first guidance resolveFrozenVersion produces.
func TestKeyringTrustPlan_UnknownPlanErrors(t *testing.T) {
	withTempStore(t)
	withTempHome(t)
	var out, errBuf bytes.Buffer
	code := runKeyring([]string{"trust", "--plan", "nonexistent"}, &out, &errBuf)
	if code == 0 {
		t.Fatal("an unknown plan must error")
	}
	if !strings.Contains(errBuf.String(), "no frozen versions") {
		t.Errorf("stderr = %q, want a no-frozen-versions error", errBuf.String())
	}
}

// TestKeyringTrust_AlreadyTrustedSurfacesPerRepoMismatch is the #2136 item-3
// regression: when `keyring trust <authority>` re-matches an owner grant (the
// fetched keys/publisher.pub is already trusted at owner scope) but a per-repo
// grant under that owner registers a DIFFERENT key (e.g. a plan self-trusted
// with `--plan`), the "already trusted" output must SURFACE the divergence and
// point at `--plan`, not silently imply every plan under the owner will launch.
func TestKeyringTrust_AlreadyTrustedSurfacesPerRepoMismatch(t *testing.T) {
	withTempHome(t)
	committed, committedPEM := genTestKey(t) // the repo's keys/publisher.pub
	planKey, _ := genTestKey(t)              // a plan's private signing key (different)

	// Seed the state the bug arises in: an owner grant for the committed key,
	// plus a per-repo grant for a plan on a DIFFERENT key.
	path := cstore.DefaultKeyringPath()
	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	kr.AddOwner("github://acme", committed)
	kr.Add("github://acme/plans", planKey)
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("save keyring: %v", err)
	}

	// `keyring trust github://acme/octane` fetches the committed publisher.pub;
	// mock the raw fetch to return it so the owner grant re-matches.
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/octane/HEAD/" + publisherKeyPath: committedPEM,
	})

	var out, errBuf bytes.Buffer
	if code := runKeyring([]string{"trust", "github://acme/octane"}, &out, &errBuf); code != 0 {
		t.Fatalf("trust must succeed (already-trusted no-op); stderr=%s", errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "already trusted") {
		t.Errorf("stdout = %q, want the already-trusted line", got)
	}
	if !strings.Contains(got, "DIFFERENT key") || !strings.Contains(got, "github://acme/plans") {
		t.Errorf("stdout = %q, want a per-repo key-mismatch note naming github://acme/plans", got)
	}
	if !strings.Contains(got, "keyring trust --plan") {
		t.Errorf("stdout = %q, want the --plan remediation hint", got)
	}
}

// TestKeyringTrust_AlreadyTrustedNoMismatchStaysQuiet proves the mismatch note
// does NOT fire when every per-repo grant under the owner uses the same key as
// the owner grant (no divergence to surface).
func TestKeyringTrust_AlreadyTrustedNoMismatchStaysQuiet(t *testing.T) {
	withTempHome(t)
	committed, committedPEM := genTestKey(t)

	path := cstore.DefaultKeyringPath()
	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	kr.AddOwner("github://acme", committed)
	kr.Add("github://acme/plans", committed) // same key, no divergence
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("save keyring: %v", err)
	}

	withMockGitHubRaw(t, map[string][]byte{
		"/acme/octane/HEAD/" + publisherKeyPath: committedPEM,
	})

	var out, errBuf bytes.Buffer
	if code := runKeyring([]string{"trust", "github://acme/octane"}, &out, &errBuf); code != 0 {
		t.Fatalf("trust must succeed; stderr=%s", errBuf.String())
	}
	if strings.Contains(out.String(), "DIFFERENT key") {
		t.Errorf("stdout = %q, want no mismatch note when keys agree", out.String())
	}
}
