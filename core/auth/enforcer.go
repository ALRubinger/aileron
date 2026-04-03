package auth

import (
	"context"
	"strings"

	"github.com/ALRubinger/aileron/core/store"
)

// StoreEnforcer implements the Enforcer interface using the enterprise store.
type StoreEnforcer struct {
	enterprises store.EnterpriseStore
}

// NewStoreEnforcer creates an enforcer backed by the enterprise store.
func NewStoreEnforcer(enterprises store.EnterpriseStore) *StoreEnforcer {
	return &StoreEnforcer{enterprises: enterprises}
}

func (e *StoreEnforcer) IsProviderAllowed(ctx context.Context, enterpriseID string, provider string) (bool, error) {
	ent, err := e.enterprises.Get(ctx, enterpriseID)
	if err != nil {
		return false, err
	}
	// No restrictions configured means all providers are allowed.
	if len(ent.AllowedAuthProviders) == 0 {
		return true, nil
	}
	for _, p := range ent.AllowedAuthProviders {
		if p == provider {
			return true, nil
		}
	}
	return false, nil
}

func (e *StoreEnforcer) IsSSORequired(ctx context.Context, enterpriseID string) (bool, error) {
	ent, err := e.enterprises.Get(ctx, enterpriseID)
	if err != nil {
		return false, err
	}
	return ent.SSORequired, nil
}

func (e *StoreEnforcer) IsEmailDomainAllowed(ctx context.Context, enterpriseID string, email string) (bool, error) {
	ent, err := e.enterprises.Get(ctx, enterpriseID)
	if err != nil {
		return false, err
	}
	// No domain restrictions means all domains are allowed.
	if len(ent.AllowedEmailDomains) == 0 {
		return true, nil
	}
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false, nil
	}
	domain := strings.ToLower(parts[1])
	for _, d := range ent.AllowedEmailDomains {
		if strings.ToLower(d) == domain {
			return true, nil
		}
	}
	return false, nil
}
