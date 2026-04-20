package llm_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/llm"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

// failingConfigStore returns a non-ErrNotFound error from GetByOwner.
type failingConfigStore struct{}

func (f *failingConfigStore) Upsert(_ context.Context, cfg model.LLMConfig) (model.LLMConfig, error) {
	return cfg, nil
}
func (f *failingConfigStore) GetByOwner(_ context.Context, _ model.LLMConfigOwnerType, _ string) (model.LLMConfig, error) {
	return model.LLMConfig{}, fmt.Errorf("database connection lost")
}
func (f *failingConfigStore) Delete(_ context.Context, _ string) error { return nil }

// Verify interface compliance.
var _ store.LLMConfigStore = (*failingConfigStore)(nil)

func TestClientResolver_ServerDefault(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	resolver := llm.NewClientResolver(configs, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected non-nil clients")
	}
}

func TestClientResolver_PerUser(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	now := time.Now()
	configs.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_1",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "user-research",
		ModelSynthesis: "user-synthesis",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	v.Put(context.Background(), "llm-config/user/usr_1", []byte("user-api-key"), vault.Metadata{})

	resolver := llm.NewClientResolver(configs, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected non-nil clients from user config")
	}
}

func TestClientResolver_PerOrg(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	now := time.Now()

	// Create user with enterprise.
	users.Create(context.Background(), model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_1",
		Email:        "test@example.com",
	})

	// Create enterprise config (no user config).
	configs.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_ent",
		OwnerType:      model.LLMConfigOwnerEnterprise,
		OwnerID:        "ent_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "org-research",
		ModelSynthesis: "org-synthesis",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	v.Put(context.Background(), "llm-config/enterprise/ent_1", []byte("org-api-key"), vault.Metadata{})

	resolver := llm.NewClientResolver(configs, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected non-nil clients from org config")
	}
}

func TestClientResolver_UserOverridesOrg(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	now := time.Now()

	users.Create(context.Background(), model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_1",
		Email:        "test@example.com",
	})

	// Both user and org configs exist.
	configs.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_user",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "user-research",
		ModelSynthesis: "user-synthesis",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	configs.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_ent",
		OwnerType:      model.LLMConfigOwnerEnterprise,
		OwnerID:        "ent_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "org-research",
		ModelSynthesis: "org-synthesis",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	v.Put(context.Background(), "llm-config/user/usr_1", []byte("user-key"), vault.Metadata{})
	v.Put(context.Background(), "llm-config/enterprise/ent_1", []byte("org-key"), vault.Metadata{})

	resolver := llm.NewClientResolver(configs, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	// Should resolve to user config (user overrides org).
	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected non-nil clients")
	}
}

func TestClientResolver_VaultFailure_FallsThrough(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	now := time.Now()

	// User config exists but vault path has no key.
	configs.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_1",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "user-research",
		ModelSynthesis: "user-synthesis",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	// Note: no vault.Put — key doesn't exist.

	resolver := llm.NewClientResolver(configs, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	// Should fall through to server default.
	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected non-nil clients from server default fallback")
	}
}

func TestClientResolver_UnsupportedProvider(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	now := time.Now()
	configs.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_1",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderOpenAI,
		ModelResearch:  "gpt-4o",
		ModelSynthesis: "gpt-4o",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	v.Put(context.Background(), "llm-config/user/usr_1", []byte("openai-key"), vault.Metadata{})

	resolver := llm.NewClientResolver(configs, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	// OpenAI is not yet supported — should fall through to default.
	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected fallback to server default for unsupported provider")
	}
}

func TestClientResolver_NoDefaultKey(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	resolver := llm.NewClientResolver(configs, users, v,
		"", "haiku", "sonnet", slog.Default())

	_, _, err := resolver.Resolve(context.Background(), "usr_1")
	if err == nil {
		t.Fatal("expected error when no default API key")
	}
	if !strings.Contains(err.Error(), "no default LLM API key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClientResolver_StoreError_PropagatesFromUserLookup(t *testing.T) {
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	resolver := llm.NewClientResolver(&failingConfigStore{}, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	_, _, err := resolver.Resolve(context.Background(), "usr_1")
	if err == nil {
		t.Fatal("expected error from store failure")
	}
	if !strings.Contains(err.Error(), "checking user LLM config") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClientResolver_EmptyAPIKeyInVault(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	users := mem.NewUserStore()
	v := vault.NewMemVault()

	now := time.Now()
	configs.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_1",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "haiku",
		ModelSynthesis: "sonnet",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	// Store empty key in vault.
	v.Put(context.Background(), "llm-config/user/usr_1", []byte(""), vault.Metadata{})

	resolver := llm.NewClientResolver(configs, users, v,
		"default-key", "haiku", "sonnet", slog.Default())

	// Empty key should fall through to server default.
	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected fallback to server default")
	}
}

func TestClientResolver_NilUsersStore(t *testing.T) {
	configs := mem.NewLLMConfigStore()
	v := vault.NewMemVault()

	// No user config, no users store — should use server default.
	resolver := llm.NewClientResolver(configs, nil, v,
		"default-key", "haiku", "sonnet", slog.Default())

	research, synthesis, err := resolver.Resolve(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if research == nil || synthesis == nil {
		t.Fatal("expected server default clients")
	}
}
