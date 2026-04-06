package crypto

import (
	"bytes"
	"testing"
)

func TestGenerateKeyPair(t *testing.T) {
	priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if priv == nil {
		t.Fatal("expected non-nil private key")
	}
	if priv.PublicKey() == nil {
		t.Fatal("expected non-nil public key")
	}
}

func TestDeriveSharedSecret_Symmetric(t *testing.T) {
	// Both sides should derive the same shared secret.
	alice, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair (alice): %v", err)
	}
	bob, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair (bob): %v", err)
	}

	secretAB, err := DeriveSharedSecret(alice, bob.PublicKey())
	if err != nil {
		t.Fatalf("DeriveSharedSecret (alice→bob): %v", err)
	}
	secretBA, err := DeriveSharedSecret(bob, alice.PublicKey())
	if err != nil {
		t.Fatalf("DeriveSharedSecret (bob→alice): %v", err)
	}

	if !bytes.Equal(secretAB, secretBA) {
		t.Fatal("shared secrets should be identical regardless of direction")
	}
	if len(secretAB) != KEKLen {
		t.Fatalf("shared secret length = %d, want %d", len(secretAB), KEKLen)
	}
}

func TestDeriveSharedSecret_DifferentPeers(t *testing.T) {
	alice, _ := GenerateKeyPair()
	bob, _ := GenerateKeyPair()
	charlie, _ := GenerateKeyPair()

	secretAB, _ := DeriveSharedSecret(alice, bob.PublicKey())
	secretAC, _ := DeriveSharedSecret(alice, charlie.PublicKey())

	if bytes.Equal(secretAB, secretAC) {
		t.Fatal("shared secrets with different peers should differ")
	}
}

func TestMarshalUnmarshalPublicKey(t *testing.T) {
	priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	pub := priv.PublicKey()
	data := MarshalPublicKey(pub)

	restored, err := UnmarshalPublicKey(data)
	if err != nil {
		t.Fatalf("UnmarshalPublicKey: %v", err)
	}

	if !bytes.Equal(pub.Bytes(), restored.Bytes()) {
		t.Fatal("round-trip marshal/unmarshal should preserve public key")
	}
}

func TestUnmarshalPublicKey_Invalid(t *testing.T) {
	_, err := UnmarshalPublicKey([]byte{0x01, 0x02, 0x03})
	if err == nil {
		t.Fatal("expected error for invalid public key bytes")
	}
}

func TestECDH_FullKeyExchangeWithEncryption(t *testing.T) {
	// Simulate the full flow: key exchange → derive shared secret → encrypt/decrypt.
	alice, _ := GenerateKeyPair()
	bob, _ := GenerateKeyPair()

	// Exchange public keys (simulated over wire via marshal/unmarshal).
	alicePubBytes := MarshalPublicKey(alice.PublicKey())
	bobPubBytes := MarshalPublicKey(bob.PublicKey())

	alicePubRestored, _ := UnmarshalPublicKey(alicePubBytes)
	bobPubRestored, _ := UnmarshalPublicKey(bobPubBytes)

	// Both derive the same shared secret.
	aliceSecret, _ := DeriveSharedSecret(alice, bobPubRestored)
	bobSecret, _ := DeriveSharedSecret(bob, alicePubRestored)

	if !bytes.Equal(aliceSecret, bobSecret) {
		t.Fatal("shared secrets should match after wire round-trip")
	}

	// Alice encrypts, Bob decrypts using the shared secret as key.
	plaintext := []byte("hello from alice")
	ct, err := Encrypt(plaintext, aliceSecret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := Decrypt(ct, bobSecret)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}
