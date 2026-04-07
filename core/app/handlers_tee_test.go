package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/config"
	"github.com/ALRubinger/aileron/enclave"
	"github.com/ALRubinger/aileron/enclave/local"
)

func teeJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestGetTeeStatusDisabled(t *testing.T) {
	s := &apiServer{
		teeCfg: &config.TEEConfig{},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/status", nil)
	s.GetTeeStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var status api.TeeStatus
	json.NewDecoder(w.Body).Decode(&status)
	if status.Enabled {
		t.Fatal("expected enabled=false")
	}
	if status.Provider != api.None {
		t.Fatalf("expected provider none, got %q", status.Provider)
	}
}

func TestGetTeeStatusEnabled(t *testing.T) {
	s := &apiServer{
		teeCfg:   &config.TEEConfig{Provider: "local"},
		teeState: &teeState{},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/status", nil)
	s.GetTeeStatus(w, r)

	var status api.TeeStatus
	json.NewDecoder(w.Body).Decode(&status)
	if !status.Enabled {
		t.Fatal("expected enabled=true")
	}
	if status.Provider != api.Local {
		t.Fatalf("expected provider local, got %q", status.Provider)
	}
}

func TestGetTeeStatusConfidentialSpace(t *testing.T) {
	s := &apiServer{
		teeCfg:   &config.TEEConfig{Provider: "confidential-space"},
		teeState: &teeState{},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/status", nil)
	s.GetTeeStatus(w, r)

	var status api.TeeStatus
	json.NewDecoder(w.Body).Decode(&status)
	if status.Provider != api.ConfidentialSpace {
		t.Fatalf("expected confidential-space, got %q", status.Provider)
	}
}

func TestInitiateAttestationDisabled(t *testing.T) {
	s := &apiServer{}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/tee/attestation", nil)
	s.InitiateAttestation(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestInitiateAttestationLocal(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	s := &apiServer{
		enclaveClient: client,
		teeState:      &teeState{},
		teeCfg:        &config.TEEConfig{Provider: "local"},
	}

	w := httptest.NewRecorder()
	r := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	s.InitiateAttestation(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp api.TeeAttestationResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Token != "dev-ok" {
		t.Fatalf("expected dev-ok, got %q", resp.Token)
	}
	if len(resp.Nonce) == 0 {
		t.Fatal("expected non-empty nonce")
	}
	if len(resp.PublicKey) == 0 {
		t.Fatal("expected non-empty public key")
	}
}

func TestEstablishTeeSessionDisabled(t *testing.T) {
	s := &apiServer{}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/tee/session", nil)
	s.EstablishTeeSession(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestEstablishTeeSessionLocal(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	verifier := &local.DevVerifier{}

	s := &apiServer{
		enclaveClient:   client,
		enclaveVerifier: verifier,
		teeState:        &teeState{},
		teeCfg:          &config.TEEConfig{Provider: "local"},
	}

	// First attest.
	w1 := httptest.NewRecorder()
	r1 := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	s.InitiateAttestation(w1, r1)

	var attestResp api.TeeAttestationResponse
	json.NewDecoder(w1.Body).Decode(&attestResp)

	// Then establish session.
	w2 := httptest.NewRecorder()
	r2 := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", api.TeeSessionRequest{
		Nonce:     attestResp.Nonce,
		Token:     attestResp.Token,
		PublicKey: attestResp.PublicKey,
	})
	s.EstablishTeeSession(w2, r2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var sessResp api.TeeSessionResponse
	json.NewDecoder(w2.Body).Decode(&sessResp)
	if !sessResp.Verified {
		t.Fatal("expected verified=true")
	}
	if sessResp.SessionId == "" {
		t.Fatal("expected non-empty session ID")
	}
}
