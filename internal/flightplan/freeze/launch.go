package freeze

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gopkg.in/yaml.v3"
)

// VerifiedFrozen is the result of a successful frozen-unit verification. It
// carries the bytes Launch (#1511) re-parses into a typed plan, already
// proven untampered against the author signature and the recorded content
// hash.
type VerifiedFrozen struct {
	// SkillMD is the frozen SKILL.md bytes exactly as stored (lock block
	// present, contentHash included). The runtime re-parses these.
	SkillMD []byte
	// ContentHash is the verified `sha256:<hex>` content hash. It equals
	// both the recomputed canonical hash and the value recorded in the
	// frozen lock block.
	ContentHash string
	// ResolvedImages are the resolved image digest pins carried by the
	// verified manifest lock block (an `environment.image` custom base →
	// digest, or composed `environment.tools` → digest). It is empty for an
	// instruction-only or no-environment unit. The runtime boots
	// the pinned image at launch, so this field is the load-bearing bridge
	// from the signed lock to the container actually entered. It is only ever
	// populated on the verified path: a tampered digest changes the recomputed
	// content hash and refuses before this field is read.
	ResolvedImages []ImagePin
	// StepTrust is the step-keyed sealed trust section carried by the
	// verified manifest lock block: for each tool step that declared a trust
	// contract, the network reach freeze sealed from it, keyed by step id.
	// Launch/runtime read the sealed reach only from here (the verified
	// path): a tampered section changes the recomputed content hash and
	// refuses before this field is read. Nil when the frozen unit seals no
	// tool-step reach.
	StepTrust map[string]StepReach
	// SignerFingerprint is the `sha256:<hex>` fingerprint of the verified
	// author public-key PEM (the same `pubPEM` the ed25519 signature verified
	// against). It is the honest signer identity the audit trail carries:
	// there is no human signer name in the system, so the key fingerprint is
	// the stable identity value. It is only ever populated on the verified
	// path, alongside a successful signature check, so the fingerprint names
	// the key that actually attested the frozen unit.
	SignerFingerprint string
	// Publisher is the connector-style publisher authority the frozen plan is
	// attributed to (`github://owner/repo` or bare `github://owner`), copied
	// from the manifest's OWN verified lock block (the region the content-hash
	// reconstruction proved). A tampered publisher changes the recomputed hash
	// and refuses before this field is read, so the value is exactly the one
	// the signature attested. Empty for a publisher-less frozen unit. The
	// launch-time publisher-trust gate reads it (only) from here.
	Publisher string
	// SignerKey is the raw ed25519 public key the signature verified against,
	// parsed from the same `pubPEM` after Verify succeeded. It is the key whose
	// membership in the keyring the launch-time publisher-trust gate checks
	// against the declared Publisher. Nil only when parsing the verified PEM
	// fails (which cannot happen after a successful Verify).
	SignerKey ed25519.PublicKey
}

// VerifyFrozen is the Launch-time verification gate (ADR-0027, #1509/#1511).
// A frozen unit refuses to run unless both the author signature verifies and
// the recorded content hash matches the bytes on disk.
//
// It reconstructs the exact canonical byte sequence Sign signed (see
// content.go): the frozen manifest and the standalone lockfile, each with the
// self-referential `contentHash` field stripped, joined by the 0x00
// separator. It then:
//
//  1. recomputes the canonical content hash and confirms it matches the
//     `lock.contentHash` recorded in the frozen manifest (and the lockfile),
//     so a tampered manifest or lockfile is rejected before any execution;
//     and
//  2. verifies the detached ed25519 signature over those same canonical
//     bytes against the stored public key.
//
// Either failure returns a non-nil error and the frozen unit must not run.
// The function is self-contained: it needs only the four stored artifacts,
// never the private key or store access.
func VerifyFrozen(skillMD, lockfile, signature, pubPEM []byte) (VerifiedFrozen, error) {
	// The recorded hash lives in the lock block of the frozen manifest. Read
	// it so we can compare the recomputed value against the author's claim.
	recorded, err := contentHashFromManifest(skillMD)
	if err != nil {
		return VerifiedFrozen{}, err
	}
	if recorded == "" {
		return VerifiedFrozen{}, fmt.Errorf("freeze: frozen manifest has no lock.contentHash; not a frozen unit")
	}

	// Parse the standalone lockfile's lock record.
	var lockfileRecord Lockfile
	if err := yaml.Unmarshal(lockfile, &lockfileRecord); err != nil {
		return VerifiedFrozen{}, fmt.Errorf("freeze: parse frozen lockfile: %w", err)
	}

	// Parse the manifest's OWN embedded lock block. The reconstruction below
	// rebuilds each no-hash region from the artifact it belongs to (manifest
	// region from the manifest's lock, lockfile region from the standalone
	// lockfile). This is what makes a tampered manifest lock block (for
	// example a swapped resolvedImages digest) change the recomputed hash and
	// fail verification: the rebuild faithfully reflects the on-disk manifest
	// rather than healing it from the lockfile.
	manifestLock, hasLock, err := lockFromManifest(skillMD)
	if err != nil {
		return VerifiedFrozen{}, err
	}
	if !hasLock {
		return VerifiedFrozen{}, fmt.Errorf("freeze: frozen manifest has no lock block; not a frozen unit")
	}

	// The manifest's embedded lock and the standalone lockfile record the same
	// frozen unit and must agree on the content hash. A disagreement is a
	// refusal (the two artifacts are inconsistent).
	if lockfileRecord.ContentHash != "" && lockfileRecord.ContentHash != recorded {
		return VerifiedFrozen{}, fmt.Errorf(
			"freeze: lock hash mismatch: manifest records %s but the lockfile records %s; the frozen unit is inconsistent",
			recorded, lockfileRecord.ContentHash)
	}

	// Reconstruct the exact byte sequence Sign signed (see content.go): the
	// frozen manifest re-emitted with its own no-hash lock block, and the
	// standalone lockfile re-marshaled without its hash. Reusing injectLock /
	// MarshalLockfile guarantees byte-for-byte parity with freeze.
	instructionOnly, err := manifestIsInstructionOnly(skillMD)
	if err != nil {
		return VerifiedFrozen{}, err
	}
	manifestNoHash, err := injectLockMaybe(skillMD, manifestLock.withoutContentHash(), instructionOnly)
	if err != nil {
		return VerifiedFrozen{}, fmt.Errorf("freeze: reconstruct frozen manifest: %w", err)
	}
	lockNoHash, err := MarshalLockfile(lockfileRecord.withoutContentHash())
	if err != nil {
		return VerifiedFrozen{}, err
	}
	canonical := canonicalContent(manifestNoHash, lockNoHash)

	// The recomputed hash must match the recorded one (tamper detection on
	// the manifest or lockfile content).
	recomputed := contentHash(manifestNoHash, lockNoHash)
	if recomputed != recorded {
		return VerifiedFrozen{}, fmt.Errorf(
			"freeze: content hash mismatch: recorded %s but recomputed %s; the frozen unit was modified after signing",
			recorded, recomputed)
	}

	// The signature must verify over the canonical bytes (author attestation
	// and tamper detection on the signature itself).
	if err := Verify(canonical, signature, pubPEM); err != nil {
		return VerifiedFrozen{}, err
	}

	// Parse the verified public key into its raw ed25519 form so the
	// publisher-trust gate can check membership against the keyring. This runs
	// only after Verify succeeded, so pubPEM is the exact key that attested the
	// unit; a parse failure here would be an internal inconsistency (Verify
	// already parsed it), so surface it rather than returning a nil key.
	signerKey, err := parsePublicKeyPEM(pubPEM)
	if err != nil {
		return VerifiedFrozen{}, fmt.Errorf("freeze: parse verified public key: %w", err)
	}

	// Return a defensive copy of the verified manifest bytes so a later mutation
	// of the caller's slice can never change what was verified. The resolved
	// image pins are copied from the manifest's OWN lock block (the region the
	// reconstruction above proved consistent against the content hash), so the
	// digest exposed here is exactly the one the signature attested. A defensive
	// slice copy keeps the field immune to later mutation of manifestLock.
	verified := append([]byte(nil), skillMD...)
	var pins []ImagePin
	if len(manifestLock.ResolvedImages) > 0 {
		pins = append([]ImagePin(nil), manifestLock.ResolvedImages...)
	}
	// The step-keyed sealed reach is copied from the manifest's own verified
	// lock block exactly like the pins, with the hosts slices defensively
	// copied so a later mutation of manifestLock can never change what a
	// caller reads off the verified path.
	var stepTrust map[string]StepReach
	if len(manifestLock.StepTrust) > 0 {
		stepTrust = make(map[string]StepReach, len(manifestLock.StepTrust))
		for id, reach := range manifestLock.StepTrust {
			stepTrust[id] = StepReach{Hosts: append([]string(nil), reach.Hosts...)}
		}
	}
	return VerifiedFrozen{
		SkillMD:           verified,
		ContentHash:       recorded,
		ResolvedImages:    pins,
		StepTrust:         stepTrust,
		SignerFingerprint: signerFingerprint(pubPEM),
		Publisher:         manifestLock.Publisher,
		SignerKey:         signerKey,
	}, nil
}

// signerFingerprint returns the `sha256:<hex>` fingerprint of the verified
// public-key PEM. It is computed only after Verify succeeds, so the value
// names the key that actually attested the frozen unit. The same PEM bytes
// always yield the same fingerprint, and a different key yields a different
// one, which is what makes it a stable audit identity.
func signerFingerprint(pubPEM []byte) string {
	sum := sha256.Sum256(pubPEM)
	return contentHashPrefix + hex.EncodeToString(sum[:])
}

// lockFromManifest parses the `aileron.lock` block of a frozen SKILL.md into a
// typed Lockfile. The second return is false when the manifest carries no lock
// block (an unfrozen or instruction-only manifest).
func lockFromManifest(skillMD []byte) (Lockfile, bool, error) {
	front, _, _, ok := splitFrontmatter(skillMD)
	if !ok {
		return Lockfile{}, false, fmt.Errorf("freeze: frozen manifest has no YAML frontmatter")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return Lockfile{}, false, fmt.Errorf("freeze: parse frozen frontmatter: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return Lockfile{}, false, fmt.Errorf("freeze: frozen frontmatter is not a mapping")
	}
	aileron := mappingValue(doc.Content[0], "aileron")
	if aileron == nil {
		return Lockfile{}, false, nil
	}
	lockNode := mappingValue(aileron, "lock")
	if lockNode == nil {
		return Lockfile{}, false, nil
	}
	var lf Lockfile
	if err := lockNode.Decode(&lf); err != nil {
		return Lockfile{}, false, fmt.Errorf("freeze: decode manifest lock block: %w", err)
	}
	return lf, true, nil
}

// contentHashFromManifest reads the `aileron.lock.contentHash` scalar from a
// frozen SKILL.md, returning "" when the manifest carries no lock block.
func contentHashFromManifest(skillMD []byte) (string, error) {
	front, _, _, ok := splitFrontmatter(skillMD)
	if !ok {
		return "", fmt.Errorf("freeze: frozen manifest has no YAML frontmatter")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return "", fmt.Errorf("freeze: parse frozen frontmatter: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return "", fmt.Errorf("freeze: frozen frontmatter is not a mapping")
	}
	root := doc.Content[0]
	aileron := mappingValue(root, "aileron")
	if aileron == nil {
		return "", nil
	}
	lock := mappingValue(aileron, "lock")
	if lock == nil {
		return "", nil
	}
	if h := mappingValue(lock, "contentHash"); h != nil {
		return h.Value, nil
	}
	return "", nil
}

// manifestIsInstructionOnly reports whether a frozen SKILL.md carries no
// `aileron` block, mirroring manifest.Parse's InstructionOnly determination
// without importing the manifest package (freeze must not depend on it).
func manifestIsInstructionOnly(skillMD []byte) (bool, error) {
	front, _, _, ok := splitFrontmatter(skillMD)
	if !ok {
		return false, fmt.Errorf("freeze: frozen manifest has no YAML frontmatter")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return false, fmt.Errorf("freeze: parse frozen frontmatter: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return false, fmt.Errorf("freeze: frozen frontmatter is not a mapping")
	}
	return mappingValue(doc.Content[0], "aileron") == nil, nil
}
