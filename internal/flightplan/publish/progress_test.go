package publish

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
)

// TestPublishProgressNonTTYIsPlain proves that when Stdout is a captured buffer
// (non-TTY), the progress output carries no carriage returns, no ANSI escape
// sequences, and no percentage figure, and the unconditional summary and
// install hint still print. The composed push shows a labeled liveness spinner
// only.
func TestPublishProgressNonTTYIsPlain(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)

	var out bytes.Buffer
	opts := composedOptions(target, layout)
	opts.Stdout = &out
	if _, err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if strings.Contains(s, "\r") {
		t.Errorf("non-TTY output contains a carriage return; output:\n%q", s)
	}
	if strings.Contains(s, "\x1b[") {
		t.Errorf("non-TTY output contains an ANSI escape sequence; output:\n%q", s)
	}
	if strings.Contains(s, "%") {
		t.Errorf("spinner-only output must not render a percentage; output:\n%s", s)
	}
	// The liveness labels resolve to a plain success line.
	if !strings.Contains(s, "Pushing image to registry...") {
		t.Errorf("missing plain push start label; output:\n%s", s)
	}
	if !strings.Contains(s, "✓ Pushed image to registry") {
		t.Errorf("missing resolved push completion line; output:\n%s", s)
	}
	if !strings.Contains(s, "published demo") {
		t.Errorf("missing publish summary; output:\n%s", s)
	}
	if !strings.Contains(s, "Install with:") {
		t.Errorf("missing install hint; output:\n%s", s)
	}
}

// TestPublishProgressQuietSuppressesFeedback proves Quiet suppresses all
// spinner output while the unconditional summary and install hint still print
// (they are the result, not progress feedback).
func TestPublishProgressQuietSuppressesFeedback(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)

	var out bytes.Buffer
	opts := composedOptions(target, layout)
	opts.Stdout = &out
	opts.Quiet = true
	if _, err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if strings.Contains(s, "%") {
		t.Errorf("quiet run still emitted a percentage figure; output:\n%s", s)
	}
	if strings.Contains(s, "Pushing") || strings.Contains(s, "Pushed") {
		t.Errorf("quiet run emitted a progress label; output:\n%s", s)
	}
	if !strings.Contains(s, "published demo") {
		t.Errorf("quiet run dropped the publish summary; output:\n%s", s)
	}
	if !strings.Contains(s, "Install with:") {
		t.Errorf("quiet run dropped the install hint; output:\n%s", s)
	}
}

// TestPublishProgressForeignBaseIndeterminateFallback proves a foreign-base
// push shows a labeled liveness spinner: no percentage figure is emitted, no
// divide-by-zero occurs, the liveness label prints, and the push succeeds.
func TestPublishProgressForeignBaseIndeterminateFallback(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	manifest := seedImage(t, src, ociConfigBody(t, "linux", "amd64", "base"))
	if err := src.Tag(ctx, manifest, manifest.Digest.String()); err != nil {
		t.Fatalf("tag source: %v", err)
	}
	target := memory.New()

	var out bytes.Buffer
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: manifest.Digest.String()}
	res, err := Run(ctx, Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
		Target: target,
		Stdout: &out,
		SourceRepo: func(context.Context, string) (oras.ReadOnlyTarget, error) {
			return src, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BindingKind != freeze.BindingManifestDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingManifestDigest)
	}
	s := out.String()
	if strings.Contains(s, "%") {
		t.Errorf("foreign-base spinner-only path still emitted a percentage figure; output:\n%s", s)
	}
	if !strings.Contains(s, "Pushing image to registry") {
		t.Errorf("foreign-base push did not emit the liveness label; output:\n%s", s)
	}
}

// TestPublishProgressImagePushErrorFails proves an image-push failure still
// surfaces the annotated error (no swallow by the Fail path). The composed push
// rejects the index write; the run must return a push-composed-image error.
func TestPublishProgressImagePushErrorFails(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	var out bytes.Buffer
	opts := composedOptions(pushFailTarget{Store: inner, failMediaType: ocispec.MediaTypeImageIndex}, layout)
	opts.Stdout = &out
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "push composed image") {
		t.Fatalf("err = %v, want a push-composed-image error even with progress wired", err)
	}
	// The Fail path must not have written a false success line.
	if strings.Contains(out.String(), "Pushed image to registry") {
		t.Errorf("failed push emitted a success completion line; output:\n%s", out.String())
	}
}

// authFailTarget wraps a memory store but rejects Push for one media type with
// an auth/scope-shaped error message, standing in for a registry that refuses a
// write because the caller lacks push credentials/scope (e.g. ghcr.io's
// "permission_denied" or a 403). annotateRegistryAuthError must recognize this
// shape and annotate the returned error.
type authFailTarget struct {
	*memory.Store
	failMediaType string
}

func (a authFailTarget) Push(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	if desc.MediaType == a.failMediaType {
		return errors.New("permission_denied: 403 Forbidden: requested access to the resource is denied")
	}
	return a.Store.Push(ctx, desc, r)
}

// TestPublishProgressImagePushAuthErrorAnnotated proves an auth/scope-shaped
// push failure surviving the ind.Fail path still reaches the operator carrying
// the annotateRegistryAuthError hint (the registry host and `docker login`
// guidance), rather than the Fail path swallowing the annotation. This guards
// the seam between the progress indicator's Fail and the Run-level error
// wrapping: the wrap must not be lost when a push fails under progress.
func TestPublishProgressImagePushAuthErrorAnnotated(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	var out bytes.Buffer
	opts := composedOptions(authFailTarget{Store: inner, failMediaType: ocispec.MediaTypeImageIndex}, layout)
	opts.Stdout = &out
	_, err := Run(ctx, opts)
	if err == nil {
		t.Fatalf("err = nil, want an annotated auth push failure")
	}
	msg := err.Error()
	// The push-composed-image wrap survives.
	if !strings.Contains(msg, "push composed image") {
		t.Errorf("err = %v, want it to still name the push-composed-image failure", err)
	}
	// The auth annotation (registry host + docker login guidance) survives the
	// Fail path and reaches the operator.
	if !strings.Contains(msg, "docker login example.com") {
		t.Errorf("err = %v, want the annotateRegistryAuthError docker-login hint to survive the Fail path", err)
	}
	if !strings.Contains(msg, "authentication/authorization") {
		t.Errorf("err = %v, want the annotateRegistryAuthError auth hint to survive the Fail path", err)
	}
	// The raw registry rejection is preserved via %w in the chain.
	if !strings.Contains(msg, "permission_denied") {
		t.Errorf("err = %v, want the raw registry rejection preserved in the chain", err)
	}
	// The Fail path must not have written a false success line.
	if strings.Contains(out.String(), "Pushed image to registry") {
		t.Errorf("failed auth push emitted a success completion line; output:\n%s", out.String())
	}
}
