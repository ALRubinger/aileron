//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"testing"
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

	// Read body once — validateResponse and json.Decode both consume it.
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
	// POST /v1/tee/attestation should return a nonce, attestation token,
	// and the enclave's ECDH public key.
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

	// Step 2: Establish session.
	// POST /v1/tee/session with the attestation evidence should verify
	// the attestation and establish an ECDH session.
	sessResp := authedPost(t, apiURL()+"/v1/tee/session", map[string]any{
		"nonce":      attestResult["nonce"],
		"token":      attestResult["token"],
		"public_key": attestResult["public_key"],
	})
	defer sessResp.Body.Close()

	if sessResp.StatusCode != 200 {
		body, _ := io.ReadAll(sessResp.Body)
		t.Fatalf("session: expected 200, got %d: %s", sessResp.StatusCode, body)
	}

	var sessResult map[string]any
	json.NewDecoder(sessResp.Body).Decode(&sessResult)

	if sessResult["verified"] != true {
		t.Fatalf("expected verified=true, got %v", sessResult["verified"])
	}
	if sessResult["session_id"] == nil || sessResult["session_id"] == "" {
		t.Fatal("expected non-empty session_id")
	}
	if sessResult["expires_at"] == nil || sessResult["expires_at"] == "" {
		t.Fatal("expected non-empty expires_at")
	}

	// Step 3: Verify status reflects active session.
	statusResp := authedGet(t, apiURL()+"/v1/tee/status")
	defer statusResp.Body.Close()

	var status map[string]any
	json.NewDecoder(statusResp.Body).Decode(&status)

	if status["attested"] != true {
		t.Fatalf("expected attested=true after attestation, got %v", status["attested"])
	}
	if status["session_active"] != true {
		t.Fatalf("expected session_active=true after session, got %v", status["session_active"])
	}
}

// Note: testing "session without attestation" is not meaningful with the
// local provider because the DevVerifier accepts any token and the server
// shares attestation state across requests. This is a valid test only with
// a real TEE provider that rejects bad attestation evidence.
