package runtime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// frozenNoImageWithPublisher freezes the no-environment worked-example variant
// attributed to the given publisher, so the runtime stays on the in-process
// path and the publisher-trust gate has a declared publisher to enforce. An
// empty publisher freezes a publisher-less plan (gate must be skipped).
func frozenNoImageWithPublisher(t *testing.T, publisher string) store.FrozenVersion {
	t.Helper()
	keyPath := writeSigningKey(t)

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	stripped := stripEnvironment(t, string(raw))

	res, err := freeze.Run(context.Background(), []byte(stripped), freeze.Options{
		Version:        "1.0.0",
		Publisher:      publisher,
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("freeze.Run no-environment with publisher %q: %v", publisher, err)
	}
	return store.FrozenVersion{
		ID:        "test",
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}
}

// recordingVerifier is a fake runtime.PublisherVerifier. It records the last
// (publisher, key) it was asked about and returns a canned error, so gate
// ordering and fail-closed behavior are testable without the cstore-backed
// keyring.
type recordingVerifier struct {
	err          error
	called       bool
	gotPublisher string
	gotKey       ed25519.PublicKey
}

func (r *recordingVerifier) VerifyPublisher(publisher string, key ed25519.PublicKey) error {
	r.called = true
	r.gotPublisher = publisher
	r.gotKey = key
	return r.err
}

// inProcessOpts wires the full in-process pipeline (dispatcher, approver, seam,
// transforms) for the no-environment worked-example variant, so a run that
// passes the gate actually completes. It mirrors TestRun_NoEnvironmentStaysInProcess.
func inProcessOpts(t *testing.T, s *store.Store, name string) Options {
	t.Helper()
	reg := NewTransformRegistry()
	reg.Register("identity", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	return Options{
		Store:      s,
		Name:       name,
		Version:    "test",
		Dispatcher: disp,
		Approver:   &fakeApprover{decision: Decision{Approved: true}},
		Seam:       fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:      FixedClock{},
		Transforms: reg,
		OutDir:     t.TempDir(),
	}
}

// TestRun_TrustedPublisherPasses proves a plan whose declared publisher trusts
// the signing key runs to completion, and the gate was consulted with the
// declared publisher and the verified signing key.
func TestRun_TrustedPublisherPasses(t *testing.T) {
	fv := frozenNoImageWithPublisher(t, "github://acme/plans")
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	v := &recordingVerifier{err: nil}
	opts := inProcessOpts(t, s, "no-exec-env")
	opts.PublisherVerifier = v

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run with a trusted publisher: %v", err)
	}
	if !v.called {
		t.Fatal("the publisher gate must be consulted when the plan declares a publisher")
	}
	if v.gotPublisher != "github://acme/plans" {
		t.Errorf("gate saw publisher %q, want github://acme/plans", v.gotPublisher)
	}
	if len(v.gotKey) != ed25519.PublicKeySize {
		t.Errorf("gate saw signing key of length %d, want %d", len(v.gotKey), ed25519.PublicKeySize)
	}
	if !strings.HasPrefix(res.ContentHash, "sha256:") {
		t.Errorf("RunResult.ContentHash = %q", res.ContentHash)
	}
}

// TestRun_UntrustedPublisherFailsClosed proves an untrusted publisher aborts
// the run before any step: Run returns the gate error and produces no outputs.
func TestRun_UntrustedPublisherFailsClosed(t *testing.T) {
	fv := frozenNoImageWithPublisher(t, "github://acme/plans")
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	disp := &dispatchRouter{results: map[string]map[string]any{}}
	v := &recordingVerifier{err: errors.New("publisher not trusted")}
	res, err := Run(context.Background(), Options{
		Store:             s,
		Name:              "no-exec-env",
		Version:           "test",
		Dispatcher:        disp,
		PublisherVerifier: v,
	})
	if err == nil {
		t.Fatal("an untrusted publisher must fail closed")
	}
	if !strings.Contains(err.Error(), "publisher not trusted") {
		t.Errorf("error = %q, want the gate refusal", err.Error())
	}
	if len(res.StepOutputs) != 0 || len(res.AuditIDs) != 0 {
		t.Errorf("a fail-closed run must produce zero outputs, got %+v", res)
	}
	if len(disp.calls) != 0 {
		t.Errorf("no action must dispatch when the gate refuses, got %d calls", len(disp.calls))
	}
}

// TestRun_DeclaredPublisherNilVerifierSkips proves the gate is skipped when no
// verifier is wired (the CLI nils it on the image-boot re-entry), even though
// the plan declares a publisher.
func TestRun_DeclaredPublisherNilVerifierSkips(t *testing.T) {
	fv := frozenNoImageWithPublisher(t, "github://acme/plans")
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	opts := inProcessOpts(t, s, "no-exec-env")
	opts.PublisherVerifier = nil // e.g. the image-boot re-entry
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("a nil verifier must skip the gate and run: %v", err)
	}
}

// TestRun_NoPublisherNonNilVerifierSkips proves a publisher-less plan skips the
// gate even when a verifier is wired: the verifier is never consulted.
func TestRun_NoPublisherNonNilVerifierSkips(t *testing.T) {
	fv := frozenNoImageWithPublisher(t, "") // no publisher declared
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	v := &recordingVerifier{err: errors.New("must not be called")}
	opts := inProcessOpts(t, s, "no-exec-env")
	opts.PublisherVerifier = v
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("a publisher-less plan must run without the gate: %v", err)
	}
	if v.called {
		t.Error("the gate must not be consulted for a publisher-less plan")
	}
}

// TestRun_InPinnedImageWithNilVerifierReachesImageRunner proves the image-boot
// re-entry (InPinnedImage, nil verifier) reaches the runInImage path without
// gating: the CLI nils the verifier there and the host already gated before boot.
func TestRun_InPinnedImageWithNilVerifierReachesImageRunner(t *testing.T) {
	// An image-pinned unit declares an environment; freeze pins the composed
	// image. With InPinnedImage the runtime stays in-process, so the gate would
	// be the only thing that could block it. A nil verifier skips it.
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	if _, err := Run(context.Background(), Options{
		Store:             s,
		Name:              "weekly-metrics-digest",
		Version:           "test",
		InPinnedImage:     true,
		PublisherVerifier: nil,
		Dispatcher:        disp,
		Approver:          &fakeApprover{decision: Decision{Approved: true}},
		Seam:              fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:             FixedClock{},
		Transforms:        reg,
		OutDir:            t.TempDir(),
	}); err != nil {
		t.Fatalf("image-boot re-entry with a nil verifier must run in-process: %v", err)
	}
}

// TestVerifyAndDecode_PopulatesPublisherAndSignerKey proves the load path
// surfaces the declared publisher and the verified signing key onto the
// LoadedPlan for the gate to consume.
func TestVerifyAndDecode_PopulatesPublisherAndSignerKey(t *testing.T) {
	fv := frozenNoImageWithPublisher(t, "github://acme/plans")
	lp, err := verifyAndDecode(fv)
	if err != nil {
		t.Fatalf("verifyAndDecode: %v", err)
	}
	if lp.Publisher != "github://acme/plans" {
		t.Errorf("LoadedPlan.Publisher = %q, want github://acme/plans", lp.Publisher)
	}
	if len(lp.SignerKey) != ed25519.PublicKeySize {
		t.Errorf("LoadedPlan.SignerKey length = %d, want %d", len(lp.SignerKey), ed25519.PublicKeySize)
	}
}
