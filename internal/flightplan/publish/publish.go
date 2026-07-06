// Package publish implements `aileron skill publish`: pushing a frozen Flight
// Plan's composed (or base) image and its signed artifact to an OCI registry
// so a second operator on another machine can install and launch it without
// re-freezing (umbrella #1898, the write half of cross-machine sharing).
//
// Freeze stays local and offline (ADR-0027); publish is the explicit,
// network-touching act. The signed ed25519 artifact remains the root of trust:
// publish is a pass-through push of the already-signed bytes, and the consumer
// verifies signature + content hash + publisher trust at install/launch.
//
// The digest binding that lets launch (#1903) verify the pulled image against
// the signed lock branches by pin type (see freeze.BindingKind):
//
//   - Composed-tools pins (LocalTag set): the image is built locally at freeze
//     and pinned by its config digest. Publish verifies the local image's
//     config digest against the signed lock and HARD-ERRORS on a mismatch
//     BEFORE pushing, then pushes the image with docker push (which preserves
//     the config digest). Binding: config-digest.
//   - Image-only / custom-base pins (no LocalTag): the signed-lock Digest is
//     the base image's registry manifest digest. Publish copies the exact
//     bytes from the SOURCE registry with oras.Copy (which preserves the
//     manifest digest) into the target registry. Copying a docker-re-encoded
//     export would change the manifest digest and defeat the binding, so the
//     foreign-base path never routes through the docker-save ImageSource.
//     Binding: manifest-digest.
//
// All oras/registry construction lives here so cmd/aileron stays oras-free.
package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/flightplan/store"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

// reproducibleCreated is the fixed RFC3339 value written for
// org.opencontainers.image.created on the artifact manifest. oras.PackManifest
// injects a wall-clock created annotation unless the key is set explicitly;
// pinning it deterministically is what makes re-publishing the same frozen
// version byte-stable (identical referrer digest), the idempotency contract.
// A zero epoch signals "reproducible, not a real build time."
const reproducibleCreated = "1970-01-01T00:00:00Z"

// ImageSource pushes a locally-built composed-tools image to the destination
// registry. Only the composed (config-digest) branch uses it; foreign-base
// pins copy from their source registry instead. Splitting inspect from push
// lets publish verify the config digest against the signed lock BEFORE any
// bytes leave the machine.
type ImageSource interface {
	// ConfigDigest returns the local composed image's config digest (its image
	// Id), so publish can fail closed before pushing a mismatched image.
	ConfigDigest(ctx context.Context, localTag string) (string, error)
	// Push tags the local image into registry under imageTag and pushes it,
	// preserving the config blob digest, so the image is resolvable in the
	// destination repository afterward.
	Push(ctx context.Context, localTag, registry, imageTag string) error
}

// Options configures a publish run.
type Options struct {
	// Name and VersionID identify the frozen version being published; VersionID
	// is also the artifact's OCI tag under Registry.
	Name      string
	VersionID string
	// Registry is the destination OCI repository (e.g. "ghcr.io/acme/plan").
	// The composed/base image and the signed-artifact referrer both land here.
	Registry string

	// Frozen is the frozen version's on-disk artifact bytes (SKILL.md, lock,
	// signature, public key). Lock is the parsed lockfile (freeze.ParseLockfile
	// over Frozen.Lockfile), carrying the image pin to publish.
	Frozen store.FrozenVersion
	Lock   freeze.Lockfile

	// ImageSource exports the composed image for the config-digest branch. Nil
	// selects the production docker-save source.
	ImageSource ImageSource
	// Target is the destination content store. Nil builds a remote.Repository
	// from Registry over the operator's docker credentials. Tests inject an
	// in-memory store.
	Target oras.Target
	// SourceRepo resolves a foreign-base pin ref to the source content store to
	// copy from. Nil builds a remote.Repository from the pin ref's registry
	// over the operator's docker credentials. Tests inject an in-memory store.
	SourceRepo func(ctx context.Context, ref string) (oras.ReadOnlyTarget, error)

	Stdout io.Writer
	Stderr io.Writer
}

// Result reports what a publish run pushed.
type Result struct {
	// ImageRef is the pushed image coordinate ("<registry>@<digest>").
	ImageRef string
	// ImageDigest is the pushed image's descriptor digest.
	ImageDigest string
	// ArtifactRef is the signed-artifact reference the consumer installs
	// ("<registry>:<version>").
	ArtifactRef string
	// ArtifactDigest is the artifact (referrers) manifest digest; stable across
	// idempotent re-publishes of the same frozen version.
	ArtifactDigest string
	// BindingKind is "config-digest" or "manifest-digest" (freeze.BindingKind).
	BindingKind string
}

// Errors returned for the documented failure modes.
var (
	// ErrNoImage is returned when the frozen version has no resolved image pin
	// to publish (an instruction-only plan).
	ErrNoImage = errors.New("publish: frozen version has no resolved image to publish")
	// ErrConfigDigestMismatch is returned when a composed pin's pushed image
	// config digest does not equal the signed-lock digest (a mislabeled or
	// tampered composed image). It fails closed rather than shipping a binding
	// #1903 would reject.
	ErrConfigDigestMismatch = errors.New("publish: composed image config digest does not match the signed lock")
)

// Run publishes opts.Frozen's image and signed artifact to opts.Registry.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if len(opts.Lock.ResolvedImages) == 0 {
		return Result{}, ErrNoImage
	}
	pin := opts.Lock.ResolvedImages[0]

	target := opts.Target
	if target == nil {
		repo, err := newRemoteRepository(opts.Registry)
		if err != nil {
			return Result{}, fmt.Errorf("publish: target registry %q: %w", opts.Registry, err)
		}
		target = repo
	}

	// 1. Push the image and resolve the binding-verified image descriptor.
	imageDesc, bindingKind, err := publishImage(ctx, opts, pin, target)
	if err != nil {
		return Result{}, err
	}

	// 2. Push the four signed-artifact blobs as layers.
	layers := make([]ocispec.Descriptor, 0, 4)
	for _, blob := range []struct {
		mediaType string
		data      []byte
	}{
		{freeze.MediaTypeSkillMD, opts.Frozen.SkillMD},
		{freeze.MediaTypeLock, opts.Frozen.Lockfile},
		{freeze.MediaTypeSignature, opts.Frozen.Signature},
		{freeze.MediaTypePublicKey, opts.Frozen.PublicKey},
	} {
		desc, err := pushBlob(ctx, target, blob.mediaType, blob.data)
		if err != nil {
			return Result{}, fmt.Errorf("publish: push artifact blob %s: %w", blob.mediaType, err)
		}
		layers = append(layers, desc)
	}

	// 3. Pack + push the signed-artifact referrers manifest (subject = image).
	//    Deterministic created + content-addressed layers make this idempotent.
	artifactDesc, err := oras.PackManifest(ctx, target, oras.PackManifestVersion1_1, freeze.ArtifactType, oras.PackManifestOptions{
		Subject: &imageDesc,
		Layers:  layers,
		ManifestAnnotations: map[string]string{
			ocispec.AnnotationCreated:    reproducibleCreated,
			freeze.AnnotationBindingKind: bindingKind,
			freeze.AnnotationName:        opts.Name,
			freeze.AnnotationVersion:     opts.VersionID,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("publish: pack signed artifact: %w", err)
	}

	// 4. Tag the artifact so `skill install <registry>:<version>` resolves it.
	if err := target.Tag(ctx, artifactDesc, opts.VersionID); err != nil {
		return Result{}, fmt.Errorf("publish: tag artifact %s: %w", opts.VersionID, err)
	}

	res := Result{
		ImageRef:       opts.Registry + "@" + imageDesc.Digest.String(),
		ImageDigest:    imageDesc.Digest.String(),
		ArtifactRef:    opts.Registry + ":" + opts.VersionID,
		ArtifactDigest: artifactDesc.Digest.String(),
		BindingKind:    bindingKind,
	}
	fmt.Fprintf(opts.Stdout, "published %s\n  image:    %s\n  artifact: %s (%s)\n  binding:  %s\n",
		opts.Name, res.ImageRef, res.ArtifactRef, res.ArtifactDigest, res.BindingKind)
	return res, nil
}

// publishImage pushes the pin's image into target and returns the descriptor to
// use as the artifact subject, plus the binding kind, verifying the binding.
func publishImage(ctx context.Context, opts Options, pin freeze.ImagePin, target oras.Target) (ocispec.Descriptor, string, error) {
	switch freeze.BindingKind(pin) {
	case freeze.BindingConfigDigest:
		return publishComposed(ctx, opts, pin, target)
	default:
		return publishForeignBase(ctx, opts, pin, target)
	}
}

// publishComposed verifies the local composed image's config digest against the
// signed lock, pushes it into the destination repository, and returns the
// pushed image descriptor. The config digest is checked BEFORE the push, so a
// mismatched (tampered/mislabeled) image never reaches the registry.
func publishComposed(ctx context.Context, opts Options, pin freeze.ImagePin, target oras.Target) (ocispec.Descriptor, string, error) {
	src := opts.ImageSource
	if src == nil {
		src = dockerImageSource{}
	}
	localConfig, err := src.ConfigDigest(ctx, pin.LocalTag)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: inspect composed image %q: %w", pin.LocalTag, err)
	}
	if localConfig != pin.Digest {
		return ocispec.Descriptor{}, "", fmt.Errorf("%w: local config %s, lock attested %s", ErrConfigDigestMismatch, localConfig, pin.Digest)
	}

	imageTag := freeze.ComposedImageTag(opts.VersionID)
	if err := src.Push(ctx, pin.LocalTag, opts.Registry, imageTag); err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: push composed image: %w", err)
	}
	desc, err := target.Resolve(ctx, imageTag)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: resolve pushed composed image: %w", err)
	}
	// Defense in depth: the pushed manifest's config digest must still equal
	// the signed lock (a registry must not have altered the config on push).
	// ociremote.ConfigDigest unwraps an OCI image index (the containerd image
	// store behind Docker Desktop makes `docker push` emit one wrapping the
	// single-platform image manifest plus a buildkit attestation) so the read
	// back succeeds on both the classic and containerd stores.
	pushedConfig, err := ociremote.ConfigDigest(ctx, target, desc)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: read pushed image config: %w", err)
	}
	if pushedConfig != pin.Digest {
		return ocispec.Descriptor{}, "", fmt.Errorf("%w: pushed config %s, lock attested %s", ErrConfigDigestMismatch, pushedConfig, pin.Digest)
	}
	return desc, freeze.BindingConfigDigest, nil
}

// publishForeignBase copies the exact source-registry bytes for an image-only
// or custom-base pin into target, preserving the manifest digest the lock
// attests.
func publishForeignBase(ctx context.Context, opts Options, pin freeze.ImagePin, target oras.Target) (ocispec.Descriptor, string, error) {
	if pin.Digest == "" {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: image-only pin %q has no digest to copy", pin.Ref)
	}
	var src oras.ReadOnlyTarget
	if opts.SourceRepo != nil {
		var err error
		if src, err = opts.SourceRepo(ctx, pin.Ref); err != nil {
			return ocispec.Descriptor{}, "", fmt.Errorf("publish: source registry for %q: %w", pin.Ref, err)
		}
	} else {
		repo, err := newRemoteRepository(pin.Ref)
		if err != nil {
			return ocispec.Descriptor{}, "", fmt.Errorf("publish: source registry for %q: %w", pin.Ref, err)
		}
		src = repo
	}
	// Copy by the attested manifest digest; oras.Copy preserves it byte-for-byte.
	root, err := oras.Copy(ctx, src, pin.Digest, target, "", oras.DefaultCopyOptions)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: copy base image %s@%s: %w", pin.Ref, pin.Digest, err)
	}
	if root.Digest.String() != pin.Digest {
		// oras.Copy resolved the digest reference, so this should hold; assert
		// to fail closed if a registry ever returns a divergent manifest.
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: copied manifest digest %s != lock attested %s", root.Digest, pin.Digest)
	}
	return root, freeze.BindingManifestDigest, nil
}

// pushBlob pushes data under mediaType, treating an already-present blob as
// success so re-publishing the same version is idempotent.
func pushBlob(ctx context.Context, dst content.Storage, mediaType string, data []byte) (ocispec.Descriptor, error) {
	desc := content.NewDescriptorFromBytes(mediaType, data)
	err := dst.Push(ctx, desc, bytes.NewReader(data))
	if err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, err
	}
	return desc, nil
}
