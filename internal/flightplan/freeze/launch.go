package freeze

import (
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

	// Parse the standalone lockfile and clear its self-referential hash. The
	// resulting Lockfile drives BOTH no-hash regions so the reconstruction
	// reuses the exact freeze-time write primitives (injectLock /
	// MarshalLockfile) rather than a parallel byte editor that could drift.
	var lf Lockfile
	if err := yaml.Unmarshal(lockfile, &lf); err != nil {
		return VerifiedFrozen{}, fmt.Errorf("freeze: parse frozen lockfile: %w", err)
	}
	lockNoHashRecord := lf.withoutContentHash()

	// Reconstruct the exact byte sequence Sign signed (see content.go): the
	// frozen manifest re-emitted with a no-hash lock block, and the
	// standalone lockfile re-marshaled without its hash. Reusing injectLock /
	// MarshalLockfile guarantees byte-for-byte parity with freeze.
	instructionOnly, err := manifestIsInstructionOnly(skillMD)
	if err != nil {
		return VerifiedFrozen{}, err
	}
	manifestNoHash, err := injectLockMaybe(skillMD, lockNoHashRecord, instructionOnly)
	if err != nil {
		return VerifiedFrozen{}, fmt.Errorf("freeze: reconstruct frozen manifest: %w", err)
	}
	lockNoHash, err := MarshalLockfile(lockNoHashRecord)
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

	return VerifiedFrozen{SkillMD: skillMD, ContentHash: recorded}, nil
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
