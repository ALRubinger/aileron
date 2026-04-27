package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/model"
)

func (s *apiServer) newExecutionGrant(ctx context.Context, intent api.IntentEnvelope, grantID string, expiresAt time.Time, bounded *map[string]interface{}, userID string) (api.ExecutionGrant, error) {
	grant := api.ExecutionGrant{
		GrantId:           grantID,
		IntentId:          intent.IntentId,
		Status:            api.ExecutionGrantStatusActive,
		ExpiresAt:         expiresAt,
		BoundedParameters: bounded,
	}
	connType, provider := resolveConnector(intent.Action.Type)
	if connType != "" && provider != "" {
		connectorID := connType + "/" + provider
		grant.ConnectorId = &connectorID
	}

	params := approvedExecutionParameters(intent, bounded)
	vaultCtx := ctx
	if userID != "" {
		vaultCtx = contextWithUser(ctx, userID)
	}
	vaultPath := s.resolveCredentialVaultPath(vaultCtx, provider)
	credentialType := ""
	if secret, err := s.vault.Get(ctx, vaultPath); err == nil {
		credentialType = secret.Metadata.Type
	}

	signed, err := s.signIssuedGrantCapability(enclave.GrantCapability{
		GrantID:        grantID,
		IntentID:       intent.IntentId,
		UserID:         userID,
		VaultPath:      vaultPath,
		Provider:       provider,
		CredentialType: credentialType,
		ActionType:     intent.Action.Type,
		Parameters:     params,
		ExpiresAt:      expiresAt.UTC().Format(time.RFC3339),
		Nonce:          randomCapabilityNonce(),
	})
	if err != nil {
		return api.ExecutionGrant{}, err
	}
	raw, _ := json.Marshal(signed)
	var capability map[string]interface{}
	_ = json.Unmarshal(raw, &capability)
	grant.Capability = &capability
	return grant, nil
}

func (s *apiServer) verifyExecutionGrantCapability(grant api.ExecutionGrant, intent api.IntentEnvelope, params map[string]any, userID, vaultPath, provider, credentialType string) (*enclave.SignedGrantCapability, error) {
	if grant.Capability == nil {
		return nil, fmt.Errorf("execution grant has no issued capability")
	}
	raw, err := json.Marshal(grant.Capability)
	if err != nil {
		return nil, fmt.Errorf("encoding issued grant capability: %w", err)
	}
	var signed enclave.SignedGrantCapability
	if err := json.Unmarshal(raw, &signed); err != nil {
		return nil, fmt.Errorf("decoding issued grant capability: %w", err)
	}
	expectedUserID := signed.Capability.UserID
	if expectedUserID != "" && expectedUserID != userID {
		return nil, fmt.Errorf("issued grant capability user does not match executor")
	}
	expected := enclave.GrantCapability{
		GrantID:        grant.GrantId,
		IntentID:       grant.IntentId,
		UserID:         expectedUserID,
		VaultPath:      vaultPath,
		Provider:       provider,
		CredentialType: credentialType,
		ActionType:     intent.Action.Type,
		Parameters:     cloneParams(params),
		ExpiresAt:      grant.ExpiresAt.UTC().Format(time.RFC3339),
		Nonce:          signed.Capability.Nonce,
	}
	if !enclave.VerifySignedGrantCapability(s.grantSigningKey(), &signed, expected) {
		return nil, fmt.Errorf("issued grant capability does not match approved grant scope")
	}
	return &signed, nil
}

func (s *apiServer) signIssuedGrantCapability(capability enclave.GrantCapability) (*enclave.SignedGrantCapability, error) {
	signed, err := enclave.SignGrantCapability(s.grantSigningKey(), capability)
	if err != nil {
		return nil, fmt.Errorf("signing grant capability: %w", err)
	}
	return signed, nil
}

func (s *apiServer) grantSigningKey() []byte {
	if len(s.grantCapabilityKey) == 0 {
		s.grantCapabilityKey = make([]byte, 32)
		if _, err := rand.Read(s.grantCapabilityKey); err != nil {
			panic(fmt.Sprintf("grant capability key: %v", err))
		}
	}
	return s.grantCapabilityKey
}

func approvedExecutionParameters(intent api.IntentEnvelope, bounded *map[string]interface{}) map[string]any {
	params := buildConnectorParams(intent.Action)
	if intent.Action.Metadata != nil {
		if domainRaw, ok := (*intent.Action.Metadata)["_model_domain"]; ok {
			if domainJSON, err := json.Marshal(domainRaw); err == nil {
				var domain model.DomainAction
				if json.Unmarshal(domainJSON, &domain) == nil {
					params = buildModelDomainParams(intent.Action.Type, domain)
				}
			}
		}
	}
	if bounded != nil {
		for k, v := range *bounded {
			params[k] = v
		}
	}
	return cloneParams(params)
}

func applyIssuedCapabilityToExecuteRequest(req *enclave.ExecuteRequest, cap enclave.GrantCapability) {
	req.UserID = cap.UserID
	req.GrantID = cap.GrantID
	req.IntentID = cap.IntentID
	req.VaultPath = cap.VaultPath
	req.Provider = cap.Provider
	req.CredentialType = cap.CredentialType
	req.ActionType = cap.ActionType
	req.Parameters = cloneParams(cap.Parameters)
}

func userIDFromContext(ctx context.Context) string {
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		return claims.Subject
	}
	return ""
}

func contextWithUser(ctx context.Context, userID string) context.Context {
	claims := &auth.Claims{}
	claims.Subject = userID
	return auth.ContextWithClaims(ctx, claims)
}

func cloneParams(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	clone := make(map[string]any, len(params))
	for k, v := range params {
		clone[k] = v
	}
	return clone
}

func randomCapabilityNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("grant capability nonce: %v", err))
	}
	return hex.EncodeToString(b)
}
