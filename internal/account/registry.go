package account

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/internal/model"
)

// Registry dispatches connected account operations to the correct
// provider-specific service. It implements Service so handlers can use
// it without knowing which providers are registered.
type Registry struct {
	providers map[model.ConnectedAccountProvider]ProviderService
}

// NewRegistry creates an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[model.ConnectedAccountProvider]ProviderService),
	}
}

// Register adds a provider service for every provider it supports.
func (r *Registry) Register(svc ProviderService) {
	for _, p := range svc.Providers() {
		r.providers[p] = svc
	}
}

// ProviderFor returns the ProviderService that handles the given provider.
// Used by the TEE callback path to access OAuth introspection methods.
func (r *Registry) ProviderFor(provider model.ConnectedAccountProvider) (ProviderService, error) {
	svc, ok := r.providers[provider]
	if !ok {
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
	return svc, nil
}

// Providers returns all registered provider identifiers.
func (r *Registry) Providers() []model.ConnectedAccountProvider {
	out := make([]model.ConnectedAccountProvider, 0, len(r.providers))
	for p := range r.providers {
		out = append(out, p)
	}
	return out
}

func (r *Registry) AuthorizationURL(ctx context.Context, provider model.ConnectedAccountProvider, state, redirectURL string) (*ConnectResult, error) {
	svc, err := r.ProviderFor(provider)
	if err != nil {
		return nil, err
	}
	return svc.AuthorizationURL(ctx, provider, state, redirectURL)
}

func (r *Registry) HandleCallback(ctx context.Context, provider model.ConnectedAccountProvider, req CallbackRequest) (*CallbackResult, error) {
	svc, err := r.ProviderFor(provider)
	if err != nil {
		return nil, err
	}
	return svc.HandleCallback(ctx, provider, req)
}

func (r *Registry) List(ctx context.Context, userID string) ([]model.ConnectedAccount, error) {
	// List aggregates across all registered provider services.
	// Each service may manage its own store, so we delegate to each and merge.
	// In practice, all services share the same ConnectedAccountStore, so
	// calling any one would return all accounts. We call the first registered
	// service. If no providers are registered, return empty.
	for _, svc := range r.providers {
		return svc.List(ctx, userID)
	}
	return nil, nil
}

func (r *Registry) Get(ctx context.Context, accountID string) (model.ConnectedAccount, error) {
	// Get tries each provider service until one returns the account.
	// Since all services typically share one store, the first call succeeds.
	for _, svc := range r.providers {
		acct, err := svc.Get(ctx, accountID)
		if err == nil {
			return acct, nil
		}
	}
	return model.ConnectedAccount{}, fmt.Errorf("connected account not found: %s", accountID)
}

func (r *Registry) Disconnect(ctx context.Context, accountID string) error {
	for _, svc := range r.providers {
		err := svc.Disconnect(ctx, accountID)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("connected account not found: %s", accountID)
}
