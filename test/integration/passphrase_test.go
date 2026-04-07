//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

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

	// 2. SET passphrase.
	resp = authedPost(t, apiURL()+"/v1/users/me/passphrase", map[string]string{
		"passphrase": passphrase,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("SET passphrase: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var setResp map[string]any
	json.NewDecoder(resp.Body).Decode(&setResp)
	if setResp["has_passphrase"] != true {
		t.Fatal("expected has_passphrase=true after set")
	}
	if setResp["salt"] == nil || setResp["salt"] == "" {
		t.Fatal("expected non-empty salt after set")
	}

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

	// 4. VERIFY with correct passphrase.
	resp = authedPost(t, apiURL()+"/v1/users/me/passphrase/verify", map[string]string{
		"passphrase": passphrase,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("VERIFY correct: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var verifyResp map[string]any
	json.NewDecoder(resp.Body).Decode(&verifyResp)
	if verifyResp["valid"] != true {
		t.Fatal("expected valid=true for correct passphrase")
	}
	if verifyResp["salt"] == nil {
		t.Fatal("expected salt in response for valid verification")
	}

	// 5. VERIFY with wrong passphrase.
	resp = authedPost(t, apiURL()+"/v1/users/me/passphrase/verify", map[string]string{
		"passphrase": "wrong-passphrase-entirely",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("VERIFY wrong: expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&verifyResp)
	if verifyResp["valid"] != false {
		t.Fatal("expected valid=false for wrong passphrase")
	}

	// 6. ROTATE passphrase.
	newPassphrase := "rotated-vault-passphrase-new"
	resp = authedPost(t, apiURL()+"/v1/users/me/passphrase", map[string]string{
		"passphrase": newPassphrase,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("ROTATE: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// 7. Old passphrase should no longer verify.
	resp = authedPost(t, apiURL()+"/v1/users/me/passphrase/verify", map[string]string{
		"passphrase": passphrase,
	})
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&verifyResp)
	if verifyResp["valid"] != false {
		t.Fatal("old passphrase should no longer verify after rotation")
	}

	// 8. New passphrase should verify.
	resp = authedPost(t, apiURL()+"/v1/users/me/passphrase/verify", map[string]string{
		"passphrase": newPassphrase,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("VERIFY new: expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&verifyResp)
	if verifyResp["valid"] != true {
		t.Fatal("new passphrase should verify after rotation")
	}
}

func TestPassphrase_TooShort(t *testing.T) {
	token := ensureAuth(t)
	if token == "" {
		t.Skip("auth not enabled")
	}

	resp := authedPost(t, apiURL()+"/v1/users/me/passphrase", map[string]string{
		"passphrase": "short",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestPassphrase_VerifyBeforeSet(t *testing.T) {
	// This test uses ensureAuth which reuses the same user.
	// After TestPassphrase_SetAndVerify runs, the user already has a passphrase.
	// We test the verify endpoint with correct behavior (it should work or 404
	// depending on test ordering). Just ensure it doesn't 500.
	token := ensureAuth(t)
	if token == "" {
		t.Skip("auth not enabled")
	}

	resp := authedPost(t, apiURL()+"/v1/users/me/passphrase/verify", map[string]string{
		"passphrase": "any-passphrase-here",
	})
	defer resp.Body.Close()
	// Should be 200 (valid/invalid) or 404 (no passphrase set) — never 500.
	if resp.StatusCode == http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected 500: %s", body)
	}
}
