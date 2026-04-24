package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func generateTestKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KEKLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return key
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := generateTestKey(t)
	plaintext := []byte("the quick brown fox jumps over the lazy dog")

	ct, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := Decrypt(ct, key)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip failed: got %q, want %q", got, plaintext)
	}
}

func TestEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	key := generateTestKey(t)

	ct, err := Encrypt([]byte{}, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := Decrypt(ct, key)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if len(got) != 0 {
		t.Fatalf("expected empty plaintext, got %d bytes", len(got))
	}
}

func TestEncrypt_UniqueNonce(t *testing.T) {
	key := generateTestKey(t)
	plaintext := []byte("same plaintext")

	ct1, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct2, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if bytes.Equal(ct1, ct2) {
		t.Fatal("two encryptions of same plaintext should produce different ciphertexts (unique nonce)")
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	key1 := generateTestKey(t)
	key2 := generateTestKey(t)
	plaintext := []byte("secret data")

	ct, err := Encrypt(plaintext, key1)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	_, err = Decrypt(ct, key2)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong key")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	key := generateTestKey(t)
	plaintext := []byte("secret data")

	ct, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip a byte in the ciphertext portion (after the nonce).
	tampered := make([]byte, len(ct))
	copy(tampered, ct)
	tampered[nonceLen+1] ^= 0xFF

	_, err = Decrypt(tampered, key)
	if err == nil {
		t.Fatal("expected error when decrypting tampered ciphertext")
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	key := generateTestKey(t)
	_, err := Decrypt([]byte{0x01, 0x02}, key)
	if err == nil {
		t.Fatal("expected error for ciphertext shorter than nonce")
	}
}

func TestEncrypt_ShortKey(t *testing.T) {
	_, err := Encrypt([]byte("data"), []byte{0x01})
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestWrapUnwrapKey_RoundTrip(t *testing.T) {
	kek := generateTestKey(t)
	dek := generateTestKey(t)

	wrapped, err := WrapKey(dek, kek)
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}

	unwrapped, err := UnwrapKey(wrapped, kek)
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}

	if !bytes.Equal(unwrapped, dek) {
		t.Fatal("unwrapped DEK does not match original")
	}
}
