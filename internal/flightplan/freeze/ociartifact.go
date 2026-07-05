package freeze

// OCI distribution contract for published Flight Plans (umbrella #1898).
//
// A published plan travels as one OCI reference: the composed (or base) image,
// plus the signed artifact (SKILL.md + aileron.lock + signature.sig +
// signing-key.pub) attached as an OCI referrers manifest whose subject is the
// image. `aileron skill publish` (#1901) writes this; `aileron skill install`
// (#1902) and `aileron skill launch` (#1903) read it. The constants below are
// the shared write/read contract so the two halves never drift.
//
// This file lives in the freeze package deliberately: freeze is the leaf both
// the publish path and the runtime read path already depend on (for ImagePin),
// so the contract is importable from both without pulling the oras/registry
// dependency into the runtime.
const (
	// ArtifactType is the OCI artifactType of the signed-artifact referrers
	// manifest that carries a frozen plan's SKILL.md/lock/signature/pubkey.
	ArtifactType = "application/vnd.aileron.flightplan.artifact.v1+json"

	// MediaTypeSkillMD, MediaTypeLock, MediaTypeSignature and MediaTypePublicKey
	// are the layer media types for the four signed-artifact files, so a
	// consumer can identify each blob by descriptor without relying on order.
	MediaTypeSkillMD   = "application/vnd.aileron.flightplan.skill.v1+markdown"
	MediaTypeLock      = "application/vnd.aileron.flightplan.lock.v1+yaml"
	MediaTypeSignature = "application/vnd.aileron.flightplan.signature.v1"
	MediaTypePublicKey = "application/vnd.aileron.flightplan.pubkey.v1"

	// AnnotationBindingKind records how the published image's digest binds to
	// the signed lock (see BindingKind). It is a convenience hint on the
	// artifact manifest; the AUTHORITATIVE binding kind is derived from the
	// signature-covered pin via BindingKind, never from this annotation, so a
	// tampered annotation cannot change what launch verifies.
	AnnotationBindingKind = "ai.aileron.binding-kind"
	// AnnotationName and AnnotationVersion record the plan name and frozen
	// version id on the artifact manifest for human/registry inspection.
	AnnotationName    = "ai.aileron.flightplan.name"
	AnnotationVersion = "ai.aileron.flightplan.version"

	// BindingConfigDigest marks a composed-tools image whose signed-lock Digest
	// is the image config digest (the image was built at freeze, so its
	// attested identity is the config blob digest, preserved across export and
	// push). Launch verifies the pushed image's `.config.digest` == lock Digest.
	BindingConfigDigest = "config-digest"
	// BindingManifestDigest marks an image-only/custom-base image whose
	// signed-lock Digest is the base image's registry manifest digest. Publish
	// copies the exact source-registry bytes (oras.Copy preserves the manifest
	// digest), and launch verifies the pulled image's manifest digest == lock
	// Digest.
	BindingManifestDigest = "manifest-digest"
)

// BindingKind reports how a resolved image pin's signed-lock Digest binds to
// the published/pulled image, derived purely from the signature-covered pin:
//
//   - A composed-tools pin carries a LocalTag and a config-digest Digest
//     (freeze builds the image locally and pins its config Id; see
//     skill_freeze.go localImageDigest and ADR-0027), so it binds by config
//     digest.
//   - An image-only or custom-base pin has no LocalTag and a registry
//     manifest-digest Digest, so it binds by manifest digest.
//
// Both #1901 (publish, which additionally hard-errors if a composed pin's
// pushed config digest does not match) and #1903 (launch) derive the binding
// kind through this function so the two halves cannot disagree, and so the
// decision is rooted in the signed lock rather than a mutable annotation.
func BindingKind(p ImagePin) string {
	if p.LocalTag != "" {
		return BindingConfigDigest
	}
	return BindingManifestDigest
}
