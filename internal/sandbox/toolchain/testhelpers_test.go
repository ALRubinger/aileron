package toolchain

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
	"golang.org/x/crypto/openpgp" //nolint:staticcheck // test signing primitive mirrors the production verify path.
)

// newTestKeyring returns a fresh in-memory PGP entity plus a keyring holding its
// public half, modelling "Node signs a release with a key we trust".
func newTestKeyring(t *testing.T) (*openpgp.Entity, keyringList) {
	t.Helper()
	ent, err := openpgp.NewEntity("toolchain test", "", "test@example.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return ent, keyringList{ent}
}

// armoredDetachedSign returns an armored detached signature over msg, the same
// shape as SHASUMS256.txt.asc consumed by the nodedist verify path.
func armoredDetachedSign(t *testing.T, ent *openpgp.Entity, msg []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, ent, bytes.NewReader(msg), nil); err != nil {
		t.Fatalf("ArmoredDetachSign: %v", err)
	}
	return buf.Bytes()
}

// signedChecksumsWithHash builds a SHASUMS256.txt mapping the pinned platform's
// archive name to hash, signs it with a fresh trusted key, and returns the body,
// signature, and a keyring source func holding the signing key.
func signedChecksumsWithHash(t *testing.T, hash string, target nodedist.Target) (body, sig []byte, keyringFn func() (keyringList, error), archiveName string) {
	t.Helper()
	name, err := nodedist.ArchiveName(pinnedVersion, target)
	if err != nil {
		t.Fatalf("ArchiveName: %v", err)
	}
	body = []byte(hash + "  " + name + "\n")
	ent, kr := newTestKeyring(t)
	sig = armoredDetachedSign(t, ent, body)
	return body, sig, func() (keyringList, error) { return kr, nil }, name
}

// pinnedChecksumForLinuxX64 reads the committed tools.lock.json and returns the
// pinned linux-x64 Node checksum, so the test stages a cache entry the boundary
// VerifyNodeChecksum guard accepts. Reading the real lockfile (rather than
// hardcoding) keeps the test correct across a future lock regeneration.
func pinnedChecksumForLinuxX64(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "container", "tools.lock.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tools.lock.json: %v", err)
	}
	var lf struct {
		Node struct {
			Checksums map[string]string `json:"checksums"`
		} `json:"node"`
	}
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatalf("parse tools.lock.json: %v", err)
	}
	sum := lf.Node.Checksums["linux-x64"]
	if sum == "" {
		t.Fatal("tools.lock.json has no linux-x64 checksum")
	}
	return sum
}
