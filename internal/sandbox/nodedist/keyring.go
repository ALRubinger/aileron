package nodedist

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"

	"golang.org/x/crypto/openpgp" //nolint:staticcheck // openpgp is the GPG verification primitive; see verify.go.
)

// nodeReleaseKeyring is the trusted Node.js release public keyring, embedded so
// the managed-toolchain provisioner (#1525) ships a self-contained set of keys
// the SHASUMS256.txt signature is verified against. The file holds the armored
// public keys of the current Node release signers (the fingerprints published
// in the nodejs/node README), one armored block per key. It is the openpgp
// counterpart to cstore's embedded ed25519 keyring: trust is baked into the
// binary, not fetched at runtime.
//
//go:embed nodekeys.asc
var nodeReleaseKeyring []byte

// armoredBlockBoundary marks the start of each armored public-key block in the
// embedded asset. openpgp.ReadArmoredKeyRing decodes a single armored block, so
// the embedded file (a concatenation of per-key blocks) is split on this
// boundary and each block parsed independently before the entities are merged.
const armoredBlockBoundary = "-----BEGIN PGP PUBLIC KEY BLOCK-----"

// DefaultKeyring returns the trusted Node release public keyring parsed from the
// embedded asset. It is the keyring the managed-toolchain provisioner passes to
// Fetcher.Keyring so a fetched SHASUMS256.txt is only ever trusted after its
// signature verifies against a known Node release key.
//
// It fails closed: an empty or unparseable asset, or one that yields no
// entities, returns an error rather than an empty keyring (which would silently
// accept nothing yet also surface no diagnostic at the call site). A build that
// embeds a valid asset never hits the error path; the guard exists so a future
// asset regression is caught at construction rather than at first fetch.
func DefaultKeyring() (openpgp.EntityList, error) {
	entities, err := parseConcatenatedArmoredKeyring(nodeReleaseKeyring)
	if err != nil {
		return nil, err
	}
	if len(entities) == 0 {
		return nil, fmt.Errorf("nodedist: embedded Node release keyring is empty")
	}
	return entities, nil
}

// parseConcatenatedArmoredKeyring parses a byte slice holding one or more
// concatenated armored PGP public-key blocks into a single EntityList. Each
// block is parsed independently (openpgp.ReadArmoredKeyRing reads only the first
// armored block in its reader), so the asset can hold the several Node release
// signers as separate exports without a merge step at generation time.
//
// A block the in-tree openpgp implementation cannot decode is skipped rather
// than failing the whole keyring. The frozen golang.org/x/crypto/openpgp
// package does not support newer public-key algorithms (e.g. EdDSA, packet type
// 22) that some current Node signers have rotated to, so a strict parse would
// reject the entire asset the moment one such key is present. Skipping the
// unparseable block keeps every RSA signer (including the one that signed the
// pinned Node release) usable; the empty-result guard in DefaultKeyring still
// fails closed if nothing parses. A real-release verification test asserts the
// surviving keys actually verify the pinned version, so a skip can never
// silently drop the key that matters.
func parseConcatenatedArmoredKeyring(asc []byte) (openpgp.EntityList, error) {
	text := string(asc)
	idx := strings.Index(text, armoredBlockBoundary)
	if idx < 0 {
		return nil, fmt.Errorf("nodedist: keyring asset has no armored public-key block")
	}
	var all openpgp.EntityList
	for idx >= 0 {
		rest := text[idx+len(armoredBlockBoundary):]
		next := strings.Index(rest, armoredBlockBoundary)
		var block string
		if next < 0 {
			block = text[idx:]
			idx = -1
		} else {
			block = text[idx : idx+len(armoredBlockBoundary)+next]
			idx = idx + len(armoredBlockBoundary) + next
		}
		el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader([]byte(block)))
		if err != nil {
			// Unparseable (unsupported algorithm) block: skip it. See doc above.
			continue
		}
		all = append(all, el...)
	}
	return all, nil
}
