//go:build integration

package integration

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/ALRubinger/aileron/core/crypto"
)

// kekVerificationConstant must match the client-side constant.
var kekVerificationConstant = []byte("aileron-kek-verification-ok")

// clientDeriveAndSet simulates the client-side SetPassphrase flow:
// generate salt, derive KEK, encrypt verification constant, POST to server.
func clientDeriveAndSet(t *testing.T, passphrase string) {
	t.Helper()

	salt, err := crypto.GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	kek, err := crypto.DeriveKEK([]byte(passphrase), salt)
	if err != nil {
		t.Fatalf("DeriveKEK: %v", err)
	}
	verification, err := crypto.Encrypt(kekVerificationConstant, kek)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	resp := authedPost(t, apiURL()+"/v1/users/me/passphrase", map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(salt),
		"kek_verification": base64.StdEncoding.EncodeToString(verification),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("SET passphrase: expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestPassphrase_SetAndVerify(t *testing.T) {
	token := ensureAuth(t)
	if token == "" {
		t.Skip("auth not enabled — passphrase endpoints require auth")
	}

	passphrase := "integration-test-vault-passphrase"

	// 1. GET salt — should report no passphrase set.
	resp := authedGet(t, apiURL()+"/v1/users/me/passphrase/salt")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET salt: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var saltResp map[string]any
	json.NewDecoder(resp.Body).Decode(&saltResp)
	if saltResp["has_passphrase"] != false {
		t.Fatalf("expected has_passphrase=false before set, got %v", saltResp["has_passphrase"])
	}

	// 2. SET passphrase (client-side flow).
	clientDeriveAndSet(t, passphrase)

	// 3. GET salt — should now report passphrase set.
	resp = authedGet(t, apiURL()+"/v1/users/me/passphrase/salt")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET salt after set: expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&saltResp)
	if saltResp["has_passphrase"] != true {
		t.Fatal("expected has_passphrase=true after set")
	}

	// 4. GET verification blob — should return the encrypted blob.
	resp = authedGet(t, apiURL()+"/v1/users/me/passphrase/verification")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET verification: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var verifyResp map[string]any
	json.NewDecoder(resp.Body).Decode(&verifyResp)
	if verifyResp["has_passphrase"] != true {
		t.Fatal("expected has_passphrase=true")
	}
	if verifyResp["kek_verification"] == nil || verifyResp["kek_verification"] == "" {
		t.Fatal("expected non-empty kek_verification")
	}

	// 5. Client-side verification: derive KEK, decrypt blob, check constant.
	saltB64 := saltResp["salt"].(string)
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		t.Fatalf("decoding salt: %v", err)
	}
	kek, err := crypto.DeriveKEK([]byte(passphrase), salt)
	if err != nil {
		t.Fatalf("DeriveKEK: %v", err)
	}
	verificationB64 := verifyResp["kek_verification"].(string)
	verificationBlob, err := base64.StdEncoding.DecodeString(verificationB64)
	if err != nil {
		t.Fatalf("decoding verification: %v", err)
	}
	plaintext, err := crypto.Decrypt(verificationBlob, kek)
	if err != nil {
		t.Fatalf("decrypting verification blob: %v", err)
	}
	if string(plaintext) != string(kekVerificationConstant) {
		t.Fatalf("verification constant mismatch: got %q", plaintext)
	}

	// 6. Wrong passphrase should fail to decrypt.
	wrongKEK, _ := crypto.DeriveKEK([]byte("wrong-passphrase-entirely"), salt)
	_, err = crypto.Decrypt(verificationBlob, wrongKEK)
	if err == nil {
		t.Fatal("expected decryption to fail with wrong passphrase")
	}

	// 7. ROTATE passphrase.
	clientDeriveAndSet(t, "rotated-vault-passphrase-new")

	// 8. Old passphrase should no longer verify against new blob.
	resp = authedGet(t, apiURL()+"/v1/users/me/passphrase/verification")
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&verifyResp)
	newVerificationB64 := verifyResp["kek_verification"].(string)
	newVerificationBlob, _ := base64.StdEncoding.DecodeString(newVerificationB64)
	_, err = crypto.Decrypt(newVerificationBlob, kek) // old KEK
	if err == nil {
		t.Fatal("old KEK should not decrypt new verification blob after rotation")
	}
}

func TestPassphrase_InvalidSalt(t *testing.T) {
	token := ensureAuth(t)
	if token == "" {
		t.Skip("auth not enabled")
	}

	// Salt too short (8 bytes instead of 16).
	resp := authedPost(t, apiURL()+"/v1/users/me/passphrase", map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(make([]byte, 8)),
		"kek_verification": base64.StdEncoding.EncodeToString([]byte("blob")),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}
