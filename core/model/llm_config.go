package model

import "time"

// LLMProvider identifies the LLM service provider.
type LLMProvider string

const (
	LLMProviderAnthropic LLMProvider = "anthropic"
	LLMProviderOpenAI    LLMProvider = "openai"
)

// LLMConfigOwnerType distinguishes user-level from org-level configuration.
type LLMConfigOwnerType string

const (
	LLMConfigOwnerUser       LLMConfigOwnerType = "user"
	LLMConfigOwnerEnterprise LLMConfigOwnerType = "enterprise"
)

// LLMConfig stores per-user or per-org LLM provider configuration.
// At most one config exists per owner (unique on owner_type + owner_id).
// The API key is stored in the vault, not in this model.
type LLMConfig struct {
	ID             string             `json:"id"`              // llm_ + UUID
	OwnerType      LLMConfigOwnerType `json:"owner_type"`      // "user" or "enterprise"
	OwnerID        string             `json:"owner_id"`        // usr_xxx or enterprise ID
	Provider       LLMProvider        `json:"provider"`        // "anthropic", "openai"
	ModelResearch  string             `json:"model_research"`  // model for research round
	ModelSynthesis string             `json:"model_synthesis"` // model for synthesis round
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

// VaultPath returns the vault key where the LLM API key is stored.
func (c LLMConfig) VaultPath() string {
	return "llm-config/" + string(c.OwnerType) + "/" + c.OwnerID
}
