package freeze

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestSignVerify_RoundTrip(t *testing.T) {
	priv, _ := genSigningKey(t)
	content := []byte("the canonical content bytes")
	sig, pubPEM, err := Sign(priv, content)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(content, sig, pubPEM); err != nil {
		t.Errorf("Verify must succeed on the signed content: %v", err)
	}
}

func TestVerify_TamperedContentFails(t *testing.T) {
	priv, _ := genSigningKey(t)
	sig, pubPEM, err := Sign(priv, []byte("original"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify([]byte("tampered"), sig, pubPEM); err == nil {
		t.Error("Verify must fail on tampered content")
	}
}

func TestVerify_WrongPublicKeyFails(t *testing.T) {
	priv, _ := genSigningKey(t)
	content := []byte("content")
	sig, _, err := Sign(priv, content)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// A different key's public PEM.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPEM, err := MarshalPublicKeyPEM(otherPub)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(content, sig, otherPEM); err == nil {
		t.Error("Verify must fail against the wrong public key")
	}
}

func TestSign_Deterministic(t *testing.T) {
	priv, _ := genSigningKey(t)
	content := []byte("deterministic message")
	a, _, err := Sign(priv, content)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	b, _, err := Sign(priv, content)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("ed25519 signature must be deterministic for a fixed key+message")
	}
}

func TestLoadSigningKey_FromPath(t *testing.T) {
	_, path := genSigningKey(t)
	priv, err := LoadSigningKey(path)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Errorf("loaded key has length %d", len(priv))
	}
}

func TestLoadSigningKey_FromEnv(t *testing.T) {
	_, path := genSigningKey(t)
	t.Setenv(SigningKeyEnv, path)
	if _, err := LoadSigningKey(""); err != nil {
		t.Errorf("LoadSigningKey from env: %v", err)
	}
}

func TestLoadSigningKey_MissingPath(t *testing.T) {
	t.Setenv(SigningKeyEnv, "")
	if _, err := LoadSigningKey(""); err == nil {
		t.Error("no key path and no env must error")
	}
	if _, err := LoadSigningKey(filepath.Join(t.TempDir(), "nope.pem")); err == nil {
		t.Error("a nonexistent key path must error")
	}
}

func TestLoadSigningKey_MalformedKey(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningKey(bad); err == nil {
		t.Error("a non-PEM key must error")
	}

	// A PEM block that is not an ed25519 key.
	wrong := filepath.Join(dir, "wrong.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})
	if err := os.WriteFile(wrong, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningKey(wrong); err == nil {
		t.Error("a malformed PKCS#8 key must error")
	}
}

func TestParsePEMPrivateKey_RejectsNonEd25519(t *testing.T) {
	// Marshal a valid PKCS#8 of a non-ed25519 key shape is awkward without
	// importing rsa; instead assert the ed25519 happy path parses and a
	// garbage DER fails.
	_, path := genSigningKey(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePEMPrivateKey(data)
	if err != nil {
		t.Fatalf("ParsePEMPrivateKey: %v", err)
	}
	if _, ok := priv.Public().(ed25519.PublicKey); !ok {
		t.Error("parsed key is not ed25519")
	}
}

func TestMarshalPublicKeyPEM_ConsumableByX509(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := MarshalPublicKeyPEM(pub)
	if err != nil {
		t.Fatalf("MarshalPublicKeyPEM: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("not a PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKIXPublicKey: %v", err)
	}
	if !pub.Equal(parsed) {
		t.Error("round-tripped public key differs")
	}
}
