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
//     and pinned by its serialization-agnostic config CONTENT digest (see
//     internal/flightplan/imgconfig). Publish verifies the local image's config
//     content digest against the signed lock and HARD-ERRORS on a mismatch
//     BEFORE pushing, then pushes the image with docker push and re-verifies the
//     pushed config content digest. The content digest survives the config-blob
//     re-serialization the containerd image store performs on push (issue
//     #2014), unlike the raw config-blob sha256. Binding: config-content-digest.
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
	"regexp"
	"strings"

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
// registry. Only the composed (config-content-digest) branch uses it;
// foreign-base pins copy from their source registry instead. Splitting inspect
// from push lets publish verify the config content digest against the signed
// lock BEFORE any bytes leave the machine.
type ImageSource interface {
	// ConfigContentDigest returns the local composed image's serialization-
	// agnostic config content digest (see internal/flightplan/imgconfig), so
	// publish can fail closed before pushing a mismatched image.
	ConfigContentDigest(ctx context.Context, localTag string) (string, error)
	// Push tags the local image into registry under imageTag and pushes it, so
	// the image is resolvable in the destination repository afterward. The
	// content digest (unlike the config-blob digest) is preserved across the
	// containerd store's push-time re-serialization by construction.
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
	// BindingKind is "config-content-digest" or "manifest-digest"
	// (freeze.BindingKind).
	BindingKind string
}

// Errors returned for the documented failure modes.
var (
	// ErrNoImage is returned when the frozen version has no resolved image pin
	// to publish (an instruction-only plan).
	ErrNoImage = errors.New("publish: frozen version has no resolved image to publish")
	// ErrConfigContentDigestMismatch is returned when a composed pin's local or
	// pushed image config CONTENT digest does not equal the signed-lock digest (a
	// mislabeled or tampered composed image). It fails closed rather than shipping
	// a binding #1903 would reject. Because the check is content-based, the
	// benign config-blob re-serialization the containerd store performs on push
	// does not trigger it; only a genuine execution-relevant field change does.
	ErrConfigContentDigestMismatch = errors.New("publish: composed image config content digest does not match the signed lock")
)

// Run publishes opts.Frozen's image and signed artifact to opts.Registry. It is
// a thin wrapper over run that pipes any returned error through
// annotateRegistryAuthError, so a registry auth/scope rejection (e.g. ghcr.io's
// raw "permission_denied ... expected scopes" from docker push) reaches the
// operator with an actionable hint instead of the opaque registry string.
// Applying it at this boundary covers auth failures from every push path
// underneath: the composed-image push, the foreign-base copy, and the
// artifact-blob pushes.
func Run(ctx context.Context, opts Options) (Result, error) {
	res, err := run(ctx, opts)
	if err != nil {
		return res, annotateRegistryAuthError(err, opts.Registry)
	}
	return res, nil
}

// run does the actual publish; Run wraps its error for operator-facing guidance.
func run(ctx context.Context, opts Options) (Result, error) {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
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

	// 4. Tag the artifact. The 16-hex content-hash slug (VersionID) is the
	//    canonical immutable coordinate `skill install <registry>:<version>`
	//    resolves. Additionally (re)point the mutable `latest` tag at this newest
	//    artifact, and — when the frozen version carries a semver label — tag
	//    under it too, so install can default to `latest` or resolve a
	//    human-facing version. A moving tag records the content-hash slug in the
	//    manifest's version annotation, so install keys the on-disk id off that
	//    resolved slug rather than the mutable tag (which would break launch's
	//    `<versionTag>-image` resolution and the no-op re-install).
	if err := target.Tag(ctx, artifactDesc, opts.VersionID); err != nil {
		return Result{}, fmt.Errorf("publish: tag artifact %s: %w", opts.VersionID, err)
	}
	if err := target.Tag(ctx, artifactDesc, "latest"); err != nil {
		return Result{}, fmt.Errorf("publish: tag artifact latest: %w", err)
	}
	if label := opts.Lock.Version; label != "" {
		// A semver `+build` metadata suffix (or any other character outside the
		// OCI tag grammar) is not a legal tag. Skip the semver tag with a warning
		// rather than hard-failing an otherwise-valid publish; the content-hash
		// and latest tags still land.
		if isValidOCITag(label) {
			if err := target.Tag(ctx, artifactDesc, label); err != nil {
				return Result{}, fmt.Errorf("publish: tag artifact %s: %w", label, err)
			}
		} else {
			fmt.Fprintf(opts.Stderr, "warning: version label %q is not a valid OCI tag; skipping the semver tag (content-hash and latest tags still published)\n", label)
		}
	}

	res := Result{
		ImageRef:       opts.Registry + "@" + imageDesc.Digest.String(),
		ImageDigest:    imageDesc.Digest.String(),
		ArtifactRef:    opts.Registry + ":" + opts.VersionID,
		ArtifactDigest: artifactDesc.Digest.String(),
		BindingKind:    bindingKind,
	}
	// Print the content-hash ArtifactRef as the source-of-truth install
	// coordinate, plus a copyable install hint so the operator can hand the exact
	// command to a second machine.
	fmt.Fprintf(opts.Stdout, "published %s\n  image:    %s\n  artifact: %s (%s)\n  binding:  %s\nInstall with:\n  aileron skill install %s\n",
		opts.Name, res.ImageRef, res.ArtifactRef, res.ArtifactDigest, res.BindingKind, res.ArtifactRef)
	return res, nil
}

// publishImage pushes the pin's image into target and returns the descriptor to
// use as the artifact subject, plus the binding kind, verifying the binding.
func publishImage(ctx context.Context, opts Options, pin freeze.ImagePin, target oras.Target) (ocispec.Descriptor, string, error) {
	switch freeze.BindingKind(pin) {
	case freeze.BindingConfigContentDigest:
		return publishComposed(ctx, opts, pin, target)
	default:
		return publishForeignBase(ctx, opts, pin, target)
	}
}

// publishComposed verifies the local composed image's config content digest
// against the signed lock, pushes it into the destination repository, and
// returns the pushed image descriptor. The content digest is checked BEFORE the
// push, so a mismatched (tampered/mislabeled) image never reaches the registry.
func publishComposed(ctx context.Context, opts Options, pin freeze.ImagePin, target oras.Target) (ocispec.Descriptor, string, error) {
	src := opts.ImageSource
	if src == nil {
		src = dockerImageSource{}
	}
	localConfig, err := src.ConfigContentDigest(ctx, pin.LocalTag)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: inspect composed image %q: %w", pin.LocalTag, err)
	}
	if localConfig != pin.Digest {
		return ocispec.Descriptor{}, "", fmt.Errorf("%w: local config %s, lock attested %s", ErrConfigContentDigestMismatch, localConfig, pin.Digest)
	}

	imageTag := freeze.ComposedImageTag(opts.VersionID)
	if err := src.Push(ctx, pin.LocalTag, opts.Registry, imageTag); err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: push composed image: %w", err)
	}
	desc, err := target.Resolve(ctx, imageTag)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: resolve pushed composed image: %w", err)
	}
	// Defense in depth: the pushed image's config CONTENT digest must still equal
	// the signed lock. ociremote.ConfigContentDigest fetches the pushed config
	// blob and canonicalizes it, so the containerd image store's benign push-time
	// re-serialization (a different config-blob sha256 for byte-identical runtime
	// fields, issue #2014) passes while a genuine field substitution is still
	// caught. It also unwraps an OCI image index (the shape `docker push` emits
	// on the containerd store: the single-platform image manifest plus a buildkit
	// attestation) so the read-back succeeds on both the classic and containerd
	// stores.
	pushedConfig, err := ociremote.ConfigContentDigest(ctx, target, desc)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("publish: read pushed image config: %w", err)
	}
	if pushedConfig != pin.Digest {
		return ocispec.Descriptor{}, "", fmt.Errorf("%w: pushed config %s, lock attested %s", ErrConfigContentDigestMismatch, pushedConfig, pin.Digest)
	}
	return desc, freeze.BindingConfigContentDigest, nil
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

// authScopeSignal matches, case-insensitively and on word boundaries, the
// tokens that mark a registry push failure as an authentication/authorization
// problem the operator can act on (missing/expired credentials, or a token
// lacking write scope) rather than a transient network fault. Registries phrase
// these differently (Docker/OCI status codes, distribution-spec error codes,
// GitHub's "expected scopes"), so the set is deliberately broad. The `\b`
// anchors keep the numeric codes from misfiring on a digit run inside a digest,
// size, or port that merely contains "401"/"403".
var authScopeSignal = regexp.MustCompile(`(?i)\b(401|403|unauthorized|forbidden|denied|insufficient_scope|scopes)\b`)

// ociTagPattern is the OCI distribution-spec tag grammar: an initial
// [A-Za-z0-9_] followed by up to 127 of [A-Za-z0-9._-]. A semver `+build`
// metadata suffix contains `+`, which is outside this set, so a version label
// carrying build metadata is deliberately rejected as a tag.
var ociTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// isValidOCITag reports whether label is a legal OCI tag, so publish can tag a
// frozen version's semver label safely and skip an illegal one with a warning
// rather than failing the whole publish.
func isValidOCITag(label string) bool {
	return ociTagPattern.MatchString(label)
}

// annotateRegistryAuthError wraps an auth/scope registry failure with an
// actionable hint naming the registry host (and, for ghcr.io, the exact
// write:packages scope a GitHub token needs to push). The raw error is preserved
// via %w so the errors.Is chain and the registry's original message both
// survive. A nil error, or any error whose message carries no auth/scope signal
// (a network fault, a local validation error), passes through unchanged.
func annotateRegistryAuthError(err error, reg string) error {
	if err == nil {
		return nil
	}
	if !authScopeSignal.MatchString(err.Error()) {
		return err
	}
	host := registryHostname(reg)
	hint := fmt.Sprintf("registry %s rejected the push (authentication/authorization): "+
		"confirm you are logged in with `docker login %s` using a token that has push/write access",
		host, host)
	if host == "ghcr.io" {
		hint += "; a GitHub token must carry the write:packages scope"
	}
	return fmt.Errorf("%s: %w", hint, err)
}

// registryHostname returns the host[:port] of a registry reference by taking the
// segment before the first "/", so "ghcr.io/acme/plan:v1" -> "ghcr.io" and
// "localhost:5000/acme/plan" -> "localhost:5000". A bare host (no repository
// path) is returned as-is. It is intentionally forgiving: it feeds a diagnostic
// hint, never a network decision.
func registryHostname(reg string) string {
	if i := strings.IndexByte(reg, '/'); i >= 0 {
		return reg[:i]
	}
	return reg
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
