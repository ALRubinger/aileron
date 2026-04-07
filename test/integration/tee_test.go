//go:build integration

package integration

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"

	"github.com/ALRubinger/aileron/core/crypto"
)

func TestTEE_Status(t *testing.T) {
	// GET /v1/tee/status should return the TEE configuration.
	// When AILERON_TEE_PROVIDER=local (set in docker-compose), the
	// response should show enabled=true, provider=local.
	resp := authedGet(t, apiURL()+"/v1/tee/status")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decoding body: %v", err)
	}

	if status["enabled"] != true {
		t.Fatalf("expected enabled=true, got %v", status["enabled"])
	}
	if status["provider"] != "local" {
		t.Fatalf("expected provider=local, got %v", status["provider"])
	}
}

func TestTEE_AttestationFlow(t *testing.T) {
	token := ensureAuth(t)
	if token == "" {
		t.Skip("auth not enabled — TEE attestation requires auth")
	}

	// Step 1: Initiate attestation.
	attestResp := authedPost(t, apiURL()+"/v1/tee/attestation", map[string]any{})
	defer attestResp.Body.Close()

	if attestResp.StatusCode != 200 {
		body, _ := io.ReadAll(attestResp.Body)
		t.Fatalf("attestation: expected 200, got %d: %s", attestResp.StatusCode, body)
	}

	var attestResult map[string]any
	json.NewDecoder(attestResp.Body).Decode(&attestResult)

	if attestResult["token"] != "dev-ok" {
		t.Fatalf("expected token=dev-ok for local provider, got %v", attestResult["token"])
	}
	if attestResult["nonce"] == nil || attestResult["nonce"] == "" {
		t.Fatal("expected non-empty nonce")
	}
	if attestResult["public_key"] == nil || attestResult["public_key"] == "" {
		t.Fatal("expected non-empty public_key")
	}

	// Step 2: Client-side ECDH key exchange.
	// Decode the enclave's public key from the attestation response.
	enclavePubB64 := attestResult["public_key"].(string)
	enclavePubBytes, err := base64.StdEncoding.DecodeString(enclavePubB64)
	if err != nil {
		t.Fatalf("decoding enclave public key: %v", err)
	}

	enclavePub, err := crypto.UnmarshalPublicKey(enclavePubBytes)
	if err != nil {
		t.Fatalf("unmarshaling enclave public key: %v", err)
	}

	// Generate client's ephemeral ECDH key pair.
	clientKey, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generating client key pair: %v", err)
	}

	// Derive shared secret.
	sharedSecret, err := crypto.DeriveSharedSecret(clientKey, enclavePub)
	if err != nil {
		t.Fatalf("deriving shared secret: %v", err)
	}

	// Encrypt a test KEK with the shared secret.
	testKEK := make([]byte, 32)
	encryptedKEK, err := crypto.Encrypt(testKEK, sharedSecret)
	if err != nil {
		t.Fatalf("encrypting KEK: %v", err)
	}

	// Step 3: Establish session by sending the encrypted KEK and client
	// public key through the server to the enclave.
	clientPubBytes := crypto.MarshalPublicKey(clientKey.PublicKey())
	sessResp := authedPost(t, apiURL()+"/v1/tee/session", map[string]any{
		"encrypted_kek":    base64.StdEncoding.EncodeToString(encryptedKEK),
		"client_public_key": base64.StdEncoding.EncodeToString(clientPubBytes),
	})
	defer sessResp.Body.Close()

	if sessResp.StatusCode != 200 {
		body, _ := io.ReadAll(sessResp.Body)
		t.Fatalf("session: expected 200, got %d: %s", sessResp.StatusCode, body)
	}

	var sessResult map[string]any
	json.NewDecoder(sessResp.Body).Decode(&sessResult)

	if sessResult["session_id"] == nil || sessResult["session_id"] == "" {
		t.Fatal("expected non-empty session_id")
	}
	if sessResult["expires_at"] == nil || sessResult["expires_at"] == "" {
		t.Fatal("expected non-empty expires_at")
	}

	// Step 4: Verify status reflects active session.
	statusResp := authedGet(t, apiURL()+"/v1/tee/status")
	defer statusResp.Body.Close()

	var status map[string]any
	json.NewDecoder(statusResp.Body).Decode(&status)

	if status["attested"] != true {
		t.Fatalf("expected attested=true after attestation, got %v", status["attested"])
	}
}
