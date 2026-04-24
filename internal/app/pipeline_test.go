package app

import (
	"log/slog"
	"testing"

	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
)

func TestBuildDraftPipeline(t *testing.T) {
	deps := draftPipelineDeps{
		apiKey:         "sk-test-key",
		modelResearch:  "claude-haiku-4-5-20251001",
		modelSynthesis: "claude-sonnet-4-6",
		llmConfigs:     mem.NewLLMConfigStore(),
		users:          mem.NewUserStore(),
		accounts:       mem.NewConnectedAccountStore(),
		instructions:   mem.NewUserInstructionStore(),
		vault:          vault.NewMemVault(),
		sourceReg:      source.NewRegistry(),
		log:            slog.Default(),
		prompts:        draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"},
	}

	pipeline := buildDraftPipeline(deps)
	if pipeline == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

func TestBuildDraftPipeline_NilUsers(t *testing.T) {
	deps := draftPipelineDeps{
		apiKey:         "sk-test-key",
		modelResearch:  "haiku",
		modelSynthesis: "sonnet",
		llmConfigs:     mem.NewLLMConfigStore(),
		users:          nil,
		accounts:       mem.NewConnectedAccountStore(),
		instructions:   mem.NewUserInstructionStore(),
		vault:          vault.NewMemVault(),
		sourceReg:      source.NewRegistry(),
		log:            slog.Default(),
		prompts:        draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	}

	pipeline := buildDraftPipeline(deps)
	if pipeline == nil {
		t.Fatal("expected non-nil pipeline even with nil users store")
	}
}
