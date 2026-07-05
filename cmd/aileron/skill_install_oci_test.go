package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

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
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, &out, &errb); code != 0 {
		t.Fatalf("install exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Installed frozen version v1abc") {
		t.Errorf("stdout = %q, want an installed-frozen line", out.String())
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
		if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, &out, &errb); code != 0 {
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
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1abc"}, &out, &errb); code != 1 {
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
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:x"}, &out, &errb); code != 1 {
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
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, &out, &errb); code != 1 {
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
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "does not resolve to a Flight Plan signed artifact") {
		t.Errorf("stderr = %q, want the not-an-artifact message", errb.String())
	}
}

func TestRunSkillInstallOCINoNameExits1(t *testing.T) {
	withTempStore(t)
	withSilentInstallVerifier(t)
	withFakePull(t, func(_ context.Context, _ pull.Options) (pull.Result, error) {
		return pull.Result{}, pull.ErrNoName
	})
	var out, errb bytes.Buffer
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, &out, &errb); code != 1 {
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
	if code := runSkillInstall([]string{"ghcr.io/acme/plan:v1"}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "write frozen version") {
		t.Errorf("stderr = %q, want the write-frozen error", errb.String())
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
	if code := runSkillInstall([]string{dir}, &out, &errb); code != 0 {
		t.Fatalf("local install exit = %d, stderr=%s", code, errb.String())
	}
}
