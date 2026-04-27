package enclave

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"reflect"
)

// GrantCapability is the scope envelope the enclave verifies before using a
// credential for a grant-backed operation.
type GrantCapability struct {
	GrantID        string         `json:"grant_id,omitempty"`
	IntentID       string         `json:"intent_id,omitempty"`
	UserID         string         `json:"user_id,omitempty"`
	VaultPath      string         `json:"vault_path,omitempty"`
	Provider       string         `json:"provider,omitempty"`
	CredentialType string         `json:"credential_type,omitempty"`
	ActionType     string         `json:"action_type,omitempty"`
	Tool           string         `json:"tool,omitempty"`
	Parameters     map[string]any `json:"parameters,omitempty"`
	ActionTypes    []string       `json:"action_types,omitempty"`
	SourceTools    []string       `json:"source_tools,omitempty"`
	ExpiresAt      string         `json:"expires_at,omitempty"`
	Nonce          string         `json:"nonce"`
}

type SignedGrantCapability struct {
	Capability GrantCapability `json:"capability"`
	Signature  string          `json:"signature"`
}

func SignGrantCapability(key []byte, cap GrantCapability) (*SignedGrantCapability, error) {
	payload, err := json.Marshal(cap)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return &SignedGrantCapability{
		Capability: cap,
		Signature:  base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}, nil
}

func VerifyExecuteCapability(key []byte, req ExecuteRequest) bool {
	if req.Capability == nil {
		return false
	}
	expected := ExecuteGrantCapability(req, req.Capability.Capability.Nonce)
	return verifyGrantCapability(key, req.Capability, expected)
}

func VerifyEscrowStoreCapability(key []byte, req EscrowStoreRequest) bool {
	if req.Capability == nil {
		return false
	}
	expected := EscrowStoreGrantCapability(req, req.Capability.Capability.Nonce)
	return verifyGrantCapability(key, req.Capability, expected)
}

func VerifySourceExecuteCapability(key []byte, req SourceExecuteRequest) bool {
	if req.Capability == nil {
		return false
	}
	expected := SourceExecuteGrantCapability(req, req.Capability.Capability.Nonce)
	return verifyGrantCapability(key, req.Capability, expected)
}

func VerifySignedGrantCapability(key []byte, signed *SignedGrantCapability, expected GrantCapability) bool {
	return verifyGrantCapability(key, signed, expected)
}

func ExecuteGrantCapability(req ExecuteRequest, nonce string) GrantCapability {
	return GrantCapability{
		GrantID:        req.GrantID,
		IntentID:       req.IntentID,
		UserID:         req.UserID,
		VaultPath:      req.VaultPath,
		Provider:       req.Provider,
		CredentialType: req.CredentialType,
		ActionType:     req.ActionType,
		Parameters:     req.Parameters,
		Nonce:          nonce,
	}
}

func EscrowStoreGrantCapability(req EscrowStoreRequest, nonce string) GrantCapability {
	return GrantCapability{
		GrantID:        req.GrantID,
		UserID:         req.UserID,
		VaultPath:      req.VaultPath,
		Provider:       req.Provider,
		CredentialType: req.CredentialType,
		ActionTypes:    append([]string(nil), req.ActionTypes...),
		SourceTools:    append([]string(nil), req.SourceTools...),
		Parameters:     req.AllowedParameters,
		ExpiresAt:      req.ExpiresAt,
		Nonce:          nonce,
	}
}

func SourceExecuteGrantCapability(req SourceExecuteRequest, nonce string) GrantCapability {
	return GrantCapability{
		UserID:     req.UserID,
		VaultPath:  req.VaultPath,
		Provider:   req.Provider,
		Tool:       req.Tool,
		Parameters: req.Params,
		Nonce:      nonce,
	}
}

func verifyGrantCapability(key []byte, signed *SignedGrantCapability, expected GrantCapability) bool {
	if signed == nil || signed.Capability.Nonce == "" || signed.Signature == "" {
		return false
	}
	if !reflect.DeepEqual(signed.Capability, expected) {
		return false
	}
	payload, err := json.Marshal(signed.Capability)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signed.Signature), []byte(expectedSig))
}
