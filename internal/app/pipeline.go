package app

import (
	"log/slog"

	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/llm"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/vault"
)

// draftPipelineDeps groups dependencies for building a draft pipeline with
// per-user/per-org LLM resolution. Extracted for testability.
type draftPipelineDeps struct {
	apiKey         string
	modelResearch  string
	modelSynthesis string
	llmConfigs     store.LLMConfigStore
	users          store.UserStore
	accounts       store.ConnectedAccountStore
	instructions   store.UserInstructionStore
	vault          vault.Vault
	sourceReg      *source.Registry
	log            *slog.Logger
	prompts        draft.Prompts
}

// buildDraftPipeline constructs a draft.Pipeline with a ClientResolver that
// supports per-user and per-org LLM provider selection, falling back to the
// server-default API key and models.
func buildDraftPipeline(deps draftPipelineDeps) *draft.Pipeline {
	researchClient := llm.NewAnthropicClient(deps.apiKey, deps.modelResearch, llm.WithLogger(deps.log))
	synthesisClient := llm.NewAnthropicClient(deps.apiKey, deps.modelSynthesis, llm.WithLogger(deps.log))

	resolver := llm.NewClientResolver(
		deps.llmConfigs, deps.users, deps.vault,
		deps.apiKey, deps.modelResearch, deps.modelSynthesis,
		deps.log,
	)

	return draft.NewPipeline(
		researchClient, synthesisClient, deps.sourceReg,
		deps.accounts, deps.instructions, deps.vault,
		deps.log, deps.prompts,
	).WithClientResolver(resolver)
}
