package nodedist_test

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
)

const sampleChecksums = "" +
	"1111111111111111111111111111111111111111111111111111111111111111  node-v24.2.0-linux-x64.tar.gz\n" +
	"2222222222222222222222222222222222222222222222222222222222222222  node-v24.2.0-darwin-arm64.tar.gz\n" +
	"3333333333333333333333333333333333333333333333333333333333333333 *node-v24.2.0-win-x64.zip\n"

func TestVerifyAndParse_HappyPath(t *testing.T) {
	signer, keyring := newTestSigner(t)
	asc := clearsignBody(t, signer, []byte(sampleChecksums))

	v := nodedist.ChecksumVerifier{Keyring: keyring}
	sums, err := v.VerifyAndParse(asc)
	if err != nil {
		t.Fatalf("VerifyAndParse: %v", err)
	}
	if got := sums["node-v24.2.0-linux-x64.tar.gz"]; got != "1111111111111111111111111111111111111111111111111111111111111111" {
		t.Errorf("linux hash = %q", got)
	}
	// The "*" binary-mode marker must be stripped from the filename.
	if got, ok := sums["node-v24.2.0-win-x64.zip"]; !ok || got != "3333333333333333333333333333333333333333333333333333333333333333" {
		t.Errorf("win zip entry = %q ok=%v", got, ok)
	}
}

func TestVerifyAndParse_BadSignatureWrongKey(t *testing.T) {
	signer, _ := newTestSigner(t)
	_, otherKeyring := newTestSigner(t) // a different key
	asc := clearsignBody(t, signer, []byte(sampleChecksums))

	v := nodedist.ChecksumVerifier{Keyring: otherKeyring}
	sums, err := v.VerifyAndParse(asc)
	if err == nil {
		t.Fatalf("VerifyAndParse accepted a signature from an untrusted key")
	}
	if sums != nil {
		t.Fatalf("entries returned despite bad signature: %v", sums)
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrBadSignature {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyAndParse_TamperedBody(t *testing.T) {
	signer, keyring := newTestSigner(t)
	asc := clearsignBody(t, signer, []byte(sampleChecksums))

	// Flip a digit inside the clearsigned checksum body. The long run of '1's
	// only occurs in the signed message, so this corrupts the covered text and
	// the signature must no longer verify.
	tampered := bytes.Replace(asc,
		[]byte("11111111111111111111"),
		[]byte("91111111111111111111"), 1)
	if bytes.Equal(tampered, asc) {
		t.Fatal("test bug: tamper did not modify the clearsigned body")
	}

	v := nodedist.ChecksumVerifier{Keyring: keyring}
	_, err := v.VerifyAndParse(tampered)
	if err == nil {
		t.Fatalf("VerifyAndParse accepted a tampered checksums body")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrBadSignature {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyAndParse_EmptyKeyringFailsClosed(t *testing.T) {
	signer, _ := newTestSigner(t)
	asc := clearsignBody(t, signer, []byte(sampleChecksums))

	v := nodedist.ChecksumVerifier{Keyring: nil}
	_, err := v.VerifyAndParse(asc)
	if err == nil {
		t.Fatalf("VerifyAndParse accepted with no trusted keys")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrBadSignature {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

// TestVerifyAndParse_NotClearsigned proves a detached-signature input (the
// shape the verifier WRONGLY assumed before the clearsign fix) is rejected
// rather than silently mis-handled. This is the direct regression guard for the
// "expected 'PGP SIGNATURE', got: PGP SIGNED MESSAGE" production failure.
func TestVerifyAndParse_NotClearsigned(t *testing.T) {
	_, keyring := newTestSigner(t)
	v := nodedist.ChecksumVerifier{Keyring: keyring}
	_, err := v.VerifyAndParse([]byte(sampleChecksums)) // plain text, no clearsign wrapper
	if err == nil {
		t.Fatalf("VerifyAndParse accepted a non-clearsigned input")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrBadSignature {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyAndParse_MalformedLine(t *testing.T) {
	signer, keyring := newTestSigner(t)
	// Valid signature over a body with a malformed line (short digest).
	asc := clearsignBody(t, signer, []byte("deadbeef  node-v24.2.0-linux-x64.tar.gz\n"))

	v := nodedist.ChecksumVerifier{Keyring: keyring}
	_, err := v.VerifyAndParse(asc)
	if err == nil {
		t.Fatalf("VerifyAndParse accepted a malformed checksum line")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrChecksumParse {
		t.Fatalf("error = %v, want ErrChecksumParse", err)
	}
}

// TestVerifyAndParse_RealNodeReleaseFixture routes Node's genuine clearsigned
// SHASUMS256.txt.asc for the pinned version through the production verifier with
// the embedded DefaultKeyring. This is the end-to-end guard that the unit tests
// with synthetic fixtures missed: it exercises the actual on-the-wire format
// (clearsigned, not detached) against the real release keys.
func TestVerifyAndParse_RealNodeReleaseFixture(t *testing.T) {
	kr, err := nodedist.DefaultKeyring()
	if err != nil {
		t.Fatalf("DefaultKeyring: %v", err)
	}
	asc, err := os.ReadFile("testdata/SHASUMS256-v22.14.0.txt.asc")
	if err != nil {
		t.Fatalf("read release fixture: %v", err)
	}
	sums, err := nodedist.ChecksumVerifier{Keyring: kr}.VerifyAndParse(asc)
	if err != nil {
		t.Fatalf("real Node release SHASUMS256.txt.asc did not verify: %v", err)
	}
	if len(sums) == 0 {
		t.Fatal("no checksum entries parsed from the verified release file")
	}
	// Every entry must be a 64-hex sha256 keyed by a non-empty filename.
	for name, hex := range sums {
		if name == "" {
			t.Errorf("empty filename in parsed checksums")
		}
		if len(hex) != 64 {
			t.Errorf("entry %q has non-sha256 digest %q", name, hex)
		}
	}
}
