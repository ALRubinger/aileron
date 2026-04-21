package crypto

import (
	"bytes"
	"testing"
)

// Test parameters — low cost to keep tests fast.
const (
	testTime    = 1
	testMemory  = 64 // 64 KB
	testThreads = 1
)

func deriveTestKEK(t *testing.T, passphrase, salt []byte) []byte {
	t.Helper()
	kek, err := DeriveKEKWithParams(passphrase, salt, testTime, testMemory, testThreads)
	if err != nil {
		t.Fatalf("DeriveKEKWithParams: %v", err)
	}
	return kek
}

func TestDeriveKEK_Deterministic(t *testing.T) {
	salt := bytes.Repeat([]byte{0xAB}, SaltLen)
	pass := []byte("correct horse battery staple")

	k1 := deriveTestKEK(t, pass, salt)
	k2 := deriveTestKEK(t, pass, salt)

	if !bytes.Equal(k1, k2) {
		t.Fatal("same passphrase + salt should produce identical KEKs")
	}
	if len(k1) != KEKLen {
		t.Fatalf("KEK length = %d, want %d", len(k1), KEKLen)
	}
}

func TestDeriveKEK_DifferentSalt(t *testing.T) {
	pass := []byte("correct horse battery staple")
	salt1 := bytes.Repeat([]byte{0x01}, SaltLen)
	salt2 := bytes.Repeat([]byte{0x02}, SaltLen)

	k1 := deriveTestKEK(t, pass, salt1)
	k2 := deriveTestKEK(t, pass, salt2)

	if bytes.Equal(k1, k2) {
		t.Fatal("different salts should produce different KEKs")
	}
}

func TestDeriveKEK_DifferentPassphrase(t *testing.T) {
	salt := bytes.Repeat([]byte{0xAB}, SaltLen)

	k1 := deriveTestKEK(t, []byte("passphrase-one"), salt)
	k2 := deriveTestKEK(t, []byte("passphrase-two"), salt)

	if bytes.Equal(k1, k2) {
		t.Fatal("different passphrases should produce different KEKs")
	}
}

func TestDeriveKEK_EmptyPassphrase(t *testing.T) {
	salt := bytes.Repeat([]byte{0xAB}, SaltLen)
	_, err := DeriveKEKWithParams(nil, salt, testTime, testMemory, testThreads)
	if err == nil {
		t.Fatal("expected error for empty passphrase")
	}
}

func TestDeriveKEK_ShortSalt(t *testing.T) {
	_, err := DeriveKEKWithParams([]byte("pass"), []byte{0x01}, testTime, testMemory, testThreads)
	if err == nil {
		t.Fatal("expected error for short salt")
	}
}

func TestDeriveKEK_DefaultParams(t *testing.T) {
	salt := bytes.Repeat([]byte{0xAB}, SaltLen)
	pass := []byte("correct horse battery staple")

	kek, err := DeriveKEK(pass, salt)
	if err != nil {
		t.Fatalf("DeriveKEK: %v", err)
	}
	if len(kek) != KEKLen {
		t.Fatalf("KEK length = %d, want %d", len(kek), KEKLen)
	}
}

func TestGenerateRandomKEK(t *testing.T) {
	k1, err := GenerateRandomKEK()
	if err != nil {
		t.Fatalf("GenerateRandomKEK: %v", err)
	}
	if len(k1) != KEKLen {
		t.Fatalf("KEK length = %d, want %d", len(k1), KEKLen)
	}

	k2, err := GenerateRandomKEK()
	if err != nil {
		t.Fatalf("GenerateRandomKEK: %v", err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("two generated KEKs should not be equal")
	}
}

func TestGenerateRandomKEK_UsableForEncryption(t *testing.T) {
	kek, err := GenerateRandomKEK()
	if err != nil {
		t.Fatalf("GenerateRandomKEK: %v", err)
	}

	plaintext := []byte("secret data")
	ciphertext, err := Encrypt(plaintext, kek)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	decrypted, err := Decrypt(ciphertext, kek)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(plaintext, decrypted) {
		t.Fatal("decrypted data doesn't match original")
	}
}

func TestGenerateSalt(t *testing.T) {
	s1, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	if len(s1) != SaltLen {
		t.Fatalf("salt length = %d, want %d", len(s1), SaltLen)
	}

	s2, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	if bytes.Equal(s1, s2) {
		t.Fatal("two generated salts should not be equal")
	}
}
