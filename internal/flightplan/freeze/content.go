package freeze

import (
	"crypto/sha256"
	"encoding/hex"
)

// contentHashPrefix is the digest-algorithm prefix every contentHash and
// image digest carries. It matches the schema pattern `^sha256:[a-f0-9]{64}$`.
const contentHashPrefix = "sha256:"

// canonicalContent assembles the exact byte sequence the contentHash is
// taken over. The hash is load-bearing for reproducibility and
// tamper-detection, so the byte sequence is defined precisely here and
// nowhere else.
//
// The canonical input is, in order:
//
//  1. the frozen SKILL.md bytes with the `lock` block injected, but with
//     the `contentHash` field itself ABSENT from that lock block (it cannot
//     reference its own value), and with no signature present, followed by
//  2. a single 0x00 separator byte (so the two regions can never collide by
//     concatenation), followed by
//  3. the lockfile bytes, also with `contentHash` absent.
//
// Both regions therefore describe the same resolved-images/capability-set
// data. The separator and the explicit exclusion of the self-referential
// hash field are what make the hash stable across re-runs on byte-identical
// input and changed when any manifest content changes.
func canonicalContent(frozenManifestNoHash, lockfileNoHash []byte) []byte {
	buf := make([]byte, 0, len(frozenManifestNoHash)+1+len(lockfileNoHash))
	buf = append(buf, frozenManifestNoHash...)
	buf = append(buf, 0x00)
	buf = append(buf, lockfileNoHash...)
	return buf
}

// contentHash returns the `sha256:<hex>` content hash over the canonical
// byte sequence. The result matches the schema pattern for lock.contentHash.
func contentHash(frozenManifestNoHash, lockfileNoHash []byte) string {
	sum := sha256.Sum256(canonicalContent(frozenManifestNoHash, lockfileNoHash))
	return contentHashPrefix + hex.EncodeToString(sum[:])
}
