package nodedist

import (
	"bytes"
	"strings"

	"golang.org/x/crypto/openpgp" //nolint:staticcheck // openpgp is the GPG detached-signature primitive Node release verification needs; no maintained in-tree replacement.
)

// Checksums maps an artifact filename to its lowercase hex SHA-256, as
// published in a verified SHASUMS256.txt.
type Checksums map[string]string

// ChecksumVerifier verifies Node's SHASUMS256.txt against its detached PGP
// signature and exposes the verified filename → sha256hex entries. Its
// contract: a checksum is only ever returned after the signature over the
// whole file has been verified against the trusted keyring, so a caller can
// never act on an unsigned or tampered checksum.
type ChecksumVerifier struct {
	// Keyring is the set of trusted Node release public keys. A nil or empty
	// keyring causes VerifyAndParse to fail closed (every signature is
	// untrusted).
	Keyring openpgp.EntityList
}

// VerifyAndParse checks the detached armored signature sig over the
// SHASUMS256.txt bytes checksums against the verifier's keyring, and only on
// success parses and returns the verified filename → sha256hex map.
//
// Order matters and is part of the contract: the signature is verified
// first. On a bad signature (wrong key, tampered body, malformed armor) it
// returns an ErrBadSignature-kind error and no entries — nothing in the file
// is trusted. Only after the signature verifies are the lines parsed.
func (v ChecksumVerifier) VerifyAndParse(checksums, sig []byte) (Checksums, error) {
	if len(v.Keyring) == 0 {
		return nil, wrapErr(ErrBadSignature, nil, "no trusted Node release keys configured")
	}
	_, err := openpgp.CheckArmoredDetachedSignature(
		v.Keyring,
		bytes.NewReader(checksums),
		bytes.NewReader(sig),
	)
	if err != nil {
		return nil, wrapErr(ErrBadSignature, err, "SHASUMS256.txt signature did not verify")
	}
	return parseChecksums(checksums)
}

// parseChecksums decodes a SHASUMS256.txt body into a filename → sha256hex
// map. Each non-blank line is "<64-hex>  <filename>" (two-space separator,
// per coreutils sha256sum). A leading "*" binary marker on the filename is
// stripped. Blank lines are skipped; a malformed line is an error so a
// truncated or corrupt file is never silently accepted.
func parseChecksums(body []byte) (Checksums, error) {
	out := Checksums{}
	for i, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Split on the first run of spaces: "<hex>  <name>".
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			return nil, wrapErr(ErrChecksumParse, nil, "malformed checksum line %d: %q", i+1, line)
		}
		hexDigest := fields[0]
		name := strings.TrimLeft(fields[1], " ")
		name = strings.TrimPrefix(name, "*") // binary-mode marker
		if !isSHA256Hex(hexDigest) {
			return nil, wrapErr(ErrChecksumParse, nil, "malformed checksum digest on line %d: %q", i+1, hexDigest)
		}
		if name == "" {
			return nil, wrapErr(ErrChecksumParse, nil, "missing filename on checksum line %d", i+1)
		}
		out[name] = strings.ToLower(hexDigest)
	}
	if len(out) == 0 {
		return nil, wrapErr(ErrChecksumParse, nil, "SHASUMS256.txt contained no checksum entries")
	}
	return out, nil
}

// isSHA256Hex reports whether s is exactly 64 lowercase-or-uppercase hex
// characters.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
