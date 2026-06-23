package nodedist_test

import (
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
)

const sampleChecksums = "" +
	"1111111111111111111111111111111111111111111111111111111111111111  node-v24.2.0-linux-x64.tar.gz\n" +
	"2222222222222222222222222222222222222222222222222222222222222222  node-v24.2.0-darwin-arm64.tar.gz\n" +
	"3333333333333333333333333333333333333333333333333333333333333333 *node-v24.2.0-win-x64.zip\n"

func TestVerifyAndParse_HappyPath(t *testing.T) {
	signer, keyring := newTestSigner(t)
	body := []byte(sampleChecksums)
	sig := armoredDetachedSign(t, signer, body)

	v := nodedist.ChecksumVerifier{Keyring: keyring}
	sums, err := v.VerifyAndParse(body, sig)
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
	body := []byte(sampleChecksums)
	sig := armoredDetachedSign(t, signer, body)

	v := nodedist.ChecksumVerifier{Keyring: otherKeyring}
	sums, err := v.VerifyAndParse(body, sig)
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
	body := []byte(sampleChecksums)
	sig := armoredDetachedSign(t, signer, body)

	tampered := append([]byte{}, body...)
	tampered[0] = '9' // flip a digit in the first hash

	v := nodedist.ChecksumVerifier{Keyring: keyring}
	_, err := v.VerifyAndParse(tampered, sig)
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
	body := []byte(sampleChecksums)
	sig := armoredDetachedSign(t, signer, body)

	v := nodedist.ChecksumVerifier{Keyring: nil}
	_, err := v.VerifyAndParse(body, sig)
	if err == nil {
		t.Fatalf("VerifyAndParse accepted with no trusted keys")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrBadSignature {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyAndParse_MalformedLine(t *testing.T) {
	signer, keyring := newTestSigner(t)
	// Valid signature over a body with a malformed line (short digest).
	body := []byte("deadbeef  node-v24.2.0-linux-x64.tar.gz\n")
	sig := armoredDetachedSign(t, signer, body)

	v := nodedist.ChecksumVerifier{Keyring: keyring}
	_, err := v.VerifyAndParse(body, sig)
	if err == nil {
		t.Fatalf("VerifyAndParse accepted a malformed checksum line")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrChecksumParse {
		t.Fatalf("error = %v, want ErrChecksumParse", err)
	}
}
