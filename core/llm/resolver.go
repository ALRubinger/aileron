package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
)

// ClientResolver resolves which LLM Client to use for a given user.
// Resolution order: per-user config → per-org config → server default.
type ClientResolver struct {
	configs          store.LLMConfigStore
	users            store.UserStore
	vault            vault.Vault
	defaultAPIKey    string
	defaultResearch  string
	defaultSynthesis string
	log              *slog.Logger
}

// NewClientResolver creates a resolver that looks up per-user/per-org LLM
// configurations and falls back to server defaults when none are found.
func NewClientResolver(
	configs store.LLMConfigStore,
	users store.UserStore,
	v vault.Vault,
	defaultAPIKey, defaultResearch, defaultSynthesis string,
	log *slog.Logger,
) *ClientResolver {
	return &ClientResolver{
		configs:          configs,
		users:            users,
		vault:            v,
		defaultAPIKey:    defaultAPIKey,
		defaultResearch:  defaultResearch,
		defaultSynthesis: defaultSynthesis,
		log:              log,
	}
}

// Resolve returns research and synthesis LLM clients for the given user.
// It checks for a per-user config first, then per-org, then falls back
// to server defaults.
func (r *ClientResolver) Resolve(ctx context.Context, userID string) (research, synthesis Client, err error) {
	// 1. Try per-user config.
	cfg, err := r.configs.GetByOwner(ctx, model.LLMConfigOwnerUser, userID)
	if err == nil {
		research, synthesis, err = r.buildClients(ctx, cfg)
		if err == nil {
			r.log.Debug("using per-user LLM config", "user_id", userID, "provider", cfg.Provider)
			return research, synthesis, nil
		}
		r.log.Warn("per-user LLM config failed, falling through", "user_id", userID, "error", err)
	} else if !isNotFound(err) {
		return nil, nil, fmt.Errorf("checking user LLM config: %w", err)
	}

	// 2. Try per-org config.
	if r.users != nil {
		user, userErr := r.users.Get(ctx, userID)
		if userErr == nil && user.EnterpriseID != "" {
			cfg, err = r.configs.GetByOwner(ctx, model.LLMConfigOwnerEnterprise, user.EnterpriseID)
			if err == nil {
				research, synthesis, err = r.buildClients(ctx, cfg)
				if err == nil {
					r.log.Debug("using per-org LLM config",
						"user_id", userID, "enterprise_id", user.EnterpriseID, "provider", cfg.Provider)
					return research, synthesis, nil
				}
				r.log.Warn("per-org LLM config failed, falling through",
					"user_id", userID, "enterprise_id", user.EnterpriseID, "error", err)
			} else if !isNotFound(err) {
				return nil, nil, fmt.Errorf("checking org LLM config: %w", err)
			}
		}
	}

	// 3. Server defaults.
	r.log.Debug("using server default LLM config", "user_id", userID)
	return r.defaultClients()
}

// buildClients constructs LLM clients from an LLMConfig, loading the API key from vault.
func (r *ClientResolver) buildClients(ctx context.Context, cfg model.LLMConfig) (Client, Client, error) {
	secret, err := r.vault.Get(ctx, cfg.VaultPath())
	if err != nil {
		return nil, nil, fmt.Errorf("retrieving API key from vault: %w", err)
	}
	apiKey := string(secret.Value)
	if apiKey == "" {
		return nil, nil, fmt.Errorf("empty API key at vault path %s", cfg.VaultPath())
	}

	return r.newClients(cfg.Provider, apiKey, cfg.ModelResearch, cfg.ModelSynthesis)
}

// defaultClients constructs LLM clients from server-default configuration.
func (r *ClientResolver) defaultClients() (Client, Client, error) {
	if r.defaultAPIKey == "" {
		return nil, nil, fmt.Errorf("no default LLM API key configured")
	}
	return r.newClients(model.LLMProviderAnthropic, r.defaultAPIKey, r.defaultResearch, r.defaultSynthesis)
}

// newClients constructs a pair of LLM clients for the given provider.
func (r *ClientResolver) newClients(provider model.LLMProvider, apiKey, researchModel, synthesisModel string) (Client, Client, error) {
	switch provider {
	case model.LLMProviderAnthropic:
		research := NewAnthropicClient(apiKey, researchModel, WithLogger(r.log))
		synthesis := NewAnthropicClient(apiKey, synthesisModel, WithLogger(r.log))
		return research, synthesis, nil
	default:
		return nil, nil, fmt.Errorf("unsupported LLM provider: %s", provider)
	}
}

func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}
