package nodedist

import (
	"bytes"
	"os"
	"testing"

	"golang.org/x/crypto/openpgp" //nolint:staticcheck // test verification mirrors the production verify path.
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/clearsign"
)

// pinnedNodeVersion is the Node version the committed tools.lock.json pins and
// the version whose real release signature the keyring must verify. Kept in sync
// with internal/sandbox/container/tools.lock.json (the toolslock package owns the
// authoritative pin; this constant only names the testdata fixture).
const pinnedNodeVersion = "22.14.0"

func TestDefaultKeyringReturnsNonEmpty(t *testing.T) {
	kr, err := DefaultKeyring()
	if err != nil {
		t.Fatalf("DefaultKeyring: %v", err)
	}
	if len(kr) == 0 {
		t.Fatal("DefaultKeyring returned an empty keyring")
	}
}

// TestDefaultKeyringVerifiesPinnedRelease is the real regression guard: the
// embedded keyring must verify the genuine SHASUMS256.txt signature Node
// published for the pinned version. The fixture under testdata is Node's real
// clearsigned SHASUMS256.txt.asc for that version, so this proves the embedded
// keys are the actual Node release signers and that the pinned-version signer
// survived asset assembly. If Node rotates the signer for a future pin, this
// test fails until the keyring asset is refreshed.
func TestDefaultKeyringVerifiesPinnedRelease(t *testing.T) {
	kr, err := DefaultKeyring()
	if err != nil {
		t.Fatalf("DefaultKeyring: %v", err)
	}
	asc, err := os.ReadFile("testdata/SHASUMS256-v" + pinnedNodeVersion + ".txt.asc")
	if err != nil {
		t.Fatalf("read release signature fixture: %v", err)
	}
	block, _ := clearsign.Decode(asc)
	if block == nil {
		t.Fatal("release fixture is not a clearsigned message")
	}
	signer, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body)
	if err != nil {
		t.Fatalf("pinned Node %s release did not verify against the embedded keyring: %v", pinnedNodeVersion, err)
	}
	if signer == nil || signer.PrimaryKey == nil {
		t.Fatal("verification returned no signer entity")
	}
}

// TestDefaultKeyringRejectsForeignSignature proves the keyring does not verify a
// signature made by a key it does not hold. A SHASUMS-shaped message is signed
// by a freshly generated entity that is NOT in the embedded keyring; verifying
// it against DefaultKeyring must fail. This is the fail-closed half of the
// contract: only known Node release keys are ever trusted.
func TestDefaultKeyringRejectsForeignSignature(t *testing.T) {
	kr, err := DefaultKeyring()
	if err != nil {
		t.Fatalf("DefaultKeyring: %v", err)
	}
	foreign, err := openpgp.NewEntity("foreign", "", "foreign@example.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	msg := []byte("deadbeef  node-vX.Y.Z-linux-x64.tar.gz\n")
	var sig bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&sig, foreign, bytes.NewReader(msg), nil); err != nil {
		t.Fatalf("ArmoredDetachSign: %v", err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(msg), bytes.NewReader(sig.Bytes())); err == nil {
		t.Fatal("foreign signature verified against the Node release keyring; want rejection")
	}
}

// TestParseConcatenatedArmoredKeyringSkipsUnparseableBlocks asserts the
// concatenated-block parser drops a block the in-tree openpgp implementation
// cannot decode while retaining the valid ones, so a future EdDSA Node key in
// the asset does not poison the whole keyring.
func TestParseConcatenatedArmoredKeyringSkipsUnparseableBlocks(t *testing.T) {
	good, err := openpgp.NewEntity("good", "", "good@example.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var goodArmored bytes.Buffer
	w, err := armor.Encode(&goodArmored, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor writer: %v", err)
	}
	if err := good.Serialize(w); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	w.Close()

	// Sandwich a syntactically-armored but undecodable block between two valid
	// keys. The bogus block has the right boundaries but garbage base64-ish body.
	bogus := armoredBlockBoundary + "\n\nZ29vYmxlZGVnb29r\n=AAAA\n-----END PGP PUBLIC KEY BLOCK-----\n"
	asset := goodArmored.String() + "\n" + bogus + "\n" + goodArmored.String()

	el, err := parseConcatenatedArmoredKeyring([]byte(asset))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(el) != 2 {
		t.Fatalf("entities = %d, want 2 (bogus block skipped, two good keys kept)", len(el))
	}
}
