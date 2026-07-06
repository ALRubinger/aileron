package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/credreq"
	"github.com/ALRubinger/aileron/internal/flightplan/pull"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// installFrozen is a minimal verified-shaped FrozenVersion the fake pull returns.
// The store's WriteFrozen only requires a name + a single-path-segment id and
// the four byte slices; it does not re-verify, so opaque bytes are sufficient to
// exercise the CLI store-write + list path.
func installFrozen(id string) store.FrozenVersion {
	return store.FrozenVersion{
		ID:        id,
		SkillMD:   []byte("# frozen\n"),
		Lockfile:  []byte("resolvedImages: []\n"),
		Signature: []byte("sig"),
		PublicKey: []byte("pub"),
	}
}

// withFakePull swaps skillPullRun for fn and restores it after the test.
func withFakePull(t *testing.T, fn func(context.Context, pull.Options) (pull.Result, error)) {
	t.Helper()
	orig := skillPullRun
	skillPullRun = fn
	t.Cleanup(func() { skillPullRun = orig })
}

// silentVerifier makes the install verifier a no-op so the CLI test does not
// touch the operator's real keyring.
func withSilentInstallVerifier(t *testing.T) {
	t.Helper()
	orig := newInstallPublisherVerifier
	newInstallPublisherVerifier = func(io.Writer) pull.PublisherVerifier { return nil }
	t.Cleanup(func() { newInstallPublisherVerifier = orig })
}

func TestRunSkillInstallOCIWritesFrozenAndLists(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	withFakePull(t, func(_ context.Context, opts pull.Options) (pull.Result, error) {
		if opts.Ref != "ghcr.io/acme/plan:v1abc" {
			t.Errorf("pull ref = %q", opts.Ref)
		}
		if opts.Verifier != nil {
			t.Error("verifier should be the silenced nil for this test")
		}
		return pull.Result{
			Frozen:         installFrozen("v1abc"),
			Name:           "rubber-duck",
			SourceRegistry: "ghcr.io/acme/plan",
			SourceTag:      "v1abc",
		}, nil
	})

	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("install exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Installed frozen version v1abc") {
		t.Errorf("stdout = %q, want an installed-frozen line", out.String())
	}
	// The install prints a one-line pointer to the onboarding verb so the
	// operator knows how to supply the plan's credentials without hand-editing.
	if !strings.Contains(out.String(), `aileron skill bind "rubber-duck"`) {
		t.Errorf("stdout = %q, want a `skill bind` pointer line", out.String())
	}

	// The store now lists the skill and holds exactly the frozen version id.
	var listOut bytes.Buffer
	if code := runSkillList(nil, &listOut, &errb); code != 0 {
		t.Fatalf("list exit = %d", code)
	}
	if !strings.Contains(listOut.String(), "rubber-duck") {
		t.Errorf("list = %q, want rubber-duck", listOut.String())
	}
	s := store.New(skillStoreDir)
	ids, err := s.FrozenVersions("rubber-duck")
	if err != nil {
		t.Fatalf("FrozenVersions: %v", err)
	}
	if len(ids) != 1 || ids[0] != "v1abc" {
		t.Errorf("frozen ids = %v, want [v1abc]", ids)
	}

	// The install records the origin sidecar so launch (#1903) can find the
	// published image on this machine that never froze the plan.
	origin, ok, err := s.ReadOrigin("rubber-duck", "v1abc")
	if err != nil {
		t.Fatalf("ReadOrigin: %v", err)
	}
	if !ok {
		t.Fatal("install did not record an origin sidecar")
	}
	if origin.Registry != "ghcr.io/acme/plan" || origin.VersionTag != "v1abc" {
		t.Errorf("origin = %+v, want registry=ghcr.io/acme/plan tag=v1abc", origin)
	}
}

func TestRunSkillInstallOCISecondInstallIsNoOp(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{Frozen: installFrozen("v1abc"), Name: "rubber-duck"}, nil
	})
	var out, errb bytes.Buffer
	for i := 0; i < 2; i++ {
		out.Reset()
		errb.Reset()
		if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, strings.NewReader(""), &out, &errb); code != 0 {
			t.Fatalf("install %d exit = %d, stderr=%s", i, code, errb.String())
		}
	}
	// The content-addressed id dedups: still exactly one version, no error.
	ids, err := store.New(skillStoreDir).FrozenVersions("rubber-duck")
	if err != nil {
		t.Fatalf("FrozenVersions: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("frozen ids = %v, want a single deduped version", ids)
	}
}

func TestRunSkillInstallOCIPullErrorExits1(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{}, pull.ErrMissingArtifact
	})
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "no published Flight Plan") {
		t.Errorf("stderr = %q, want the missing-artifact message", errb.String())
	}
}

func TestRunSkillInstallOCIMissingTagExits1(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{}, pull.ErrMissingVersionTag
	})
	var out, errb bytes.Buffer
	// Force the OCI branch with a host-shaped ref; the fake returns the tag error.
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:x"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "tagged reference") {
		t.Errorf("stderr = %q, want the tagged-reference hint", errb.String())
	}
}

func TestRunSkillInstallOCIUntrustedPublisherExits1(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	refusal := errors.New("skill install: publisher github://acme is not trusted")
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{}, refusal
	})
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not trusted") {
		t.Errorf("stderr = %q, want the untrusted-publisher message", errb.String())
	}
	// Nothing must have landed in the store on a fail-closed install.
	names, err := store.New(skillStoreDir).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("store has %v after a refused install, want empty", names)
	}
}

func TestRunSkillInstallOCINotAnArtifactExits1(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{}, pull.ErrNotAnArtifact
	})
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "does not resolve to a Flight Plan signed artifact") {
		t.Errorf("stderr = %q, want the not-an-artifact message", errb.String())
	}
	// The error is actionable: it names the likely cause (an image / referrers
	// fallback tag) and points at the plan's version coordinate and the
	// `artifact:` line from publish.
	msg := errb.String()
	for _, want := range []string{"referrers fallback tag", "ghcr.io/owner/plan:<16-hex>", ":latest", "artifact:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr = %q, want the actionable hint to contain %q", msg, want)
		}
	}
}

func TestRunSkillInstallOCINoNameExits1(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{}, pull.ErrNoName
	})
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "declares no skill name") {
		t.Errorf("stderr = %q, want the no-name message", errb.String())
	}
}

func TestRunSkillInstallOCIWriteErrorExits1(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	// An empty ID fails store.WriteFrozen after a successful pull+verify, so the
	// CLI's write-error branch fires.
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{Frozen: installFrozen(""), Name: "rubber-duck"}, nil
	})
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "write frozen version") {
		t.Errorf("stderr = %q, want the write-frozen error", errb.String())
	}
}

// withForcedTTY forces the interactive-terminal seam on so a test exercises the
// auto-bind offer without a real TTY, restoring the package default (false, set
// in main_test.go's init) afterwards.
func withForcedTTY(t *testing.T) {
	t.Helper()
	orig := isTTYFn
	isTTYFn = func() bool { return true }
	t.Cleanup(func() { isTTYFn = orig })
}

// TestRunSkillInstallOCITTYDeclineKeepsPointer is the regression for the
// interactive auto-bind offer (#2026): when a terminal is attached but the
// operator declines with "n", the frozen/OCI install falls back to the same
// `skill bind` pointer the non-interactive path prints and writes no binding
// descriptor. Before the offer existed the install printed only the pointer; the
// decline path must preserve exactly that behavior.
func TestRunSkillInstallOCITTYDeclineKeepsPointer(t *testing.T) {
	vault := &fakeBindVault{}
	// withBindSeams sets up the temp store + descriptor path and canned reqs; the
	// deriver would yield a real requirement if bind were reached, so a descriptor
	// appearing proves the offer chained into bind.
	descPath := withBindSeams(t, []credreq.RequiredBinding{sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com")}, vault)
	withSilentInstallVerifier(t)
	withForcedTTY(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{Frozen: installFrozen("v1abc"), Name: "rubber-duck"}, nil
	})

	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, strings.NewReader("n\n"), &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	// The offer was made...
	if !strings.Contains(out.String(), "Bind \"rubber-duck\"") {
		t.Errorf("stdout = %q, want the interactive bind offer", out.String())
	}
	// ...declined, so the fallback pointer prints and nothing is bound.
	if !strings.Contains(out.String(), `aileron skill bind "rubber-duck"`) {
		t.Errorf("stdout = %q, want the bind pointer after declining", out.String())
	}
	if _, err := os.Stat(descPath); !os.IsNotExist(err) {
		t.Errorf("descriptor %q exists (stat err=%v), want none written on decline", descPath, err)
	}
	if vault.puts != 0 {
		t.Errorf("vault puts = %d, want 0 when the offer is declined", vault.puts)
	}
}

// TestRunSkillInstallOCITTYAcceptChainsToBind proves the accept path (#2026):
// with a terminal attached and the operator accepting (empty line = default
// yes), the install chains straight into bind, which onboards the plan's declared
// credentials and writes the binding descriptor. The fallback pointer must not
// print once the operator accepted.
func TestRunSkillInstallOCITTYAcceptChainsToBind(t *testing.T) {
	vault := &fakeBindVault{}
	descPath := withBindSeams(t, []credreq.RequiredBinding{sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com")}, vault)
	withSilentInstallVerifier(t)
	withForcedTTY(t)
	secretCalls := withSecret(t, "supersecret")
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{Frozen: installFrozen("v1abc"), Name: "rubber-duck"}, nil
	})

	// Empty line accepts the [Y/n] default; the next line feeds bind's own
	// access-key-id prompt, proving stdin bytes survive the offer's ReadString.
	stdin := strings.NewReader("\nAKIAIOSFODNN7EXAMPLE\n")
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, stdin, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if *secretCalls != 1 {
		t.Errorf("secret prompted %d times, want 1", *secretCalls)
	}
	if vault.puts != 1 || string(vault.services["prod-reader"]) != "supersecret" {
		t.Errorf("vault = %+v, want one put of user/prod-reader=supersecret", vault.services)
	}
	// Bind wrote the descriptor for the plan's declared identity, using the key
	// read from the shared stdin.
	d := parseDescriptor(t, descPath)
	if len(d.Bindings) != 1 || d.Bindings[0].IdentityLabel != "prod-reader" {
		t.Fatalf("descriptor bindings = %+v, want one prod-reader entry", d.Bindings)
	}
	if d.Bindings[0].AccessKeyID != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("access_key_id = %q, want the key read from the shared stdin", d.Bindings[0].AccessKeyID)
	}
	// The fallback pointer must not print once the operator accepted.
	if strings.Contains(out.String(), "to supply this plan's credentials") {
		t.Errorf("stdout = %q, must not print the fallback pointer after accepting", out.String())
	}
}

func TestRunSkillInstallLocalPathStillWorks(t *testing.T) {
	// A local path source must NOT route to the OCI branch: the fake pull would
	// fail the test if consulted.
	withTempStore(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		t.Fatal("local-path install must not reach the pull path")
		return pull.Result{}, nil
	})
	dir := writeSkillFile(t, instructionOnlySkillMd)
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{dir}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("local install exit = %d, stderr=%s", code, errb.String())
	}
}
