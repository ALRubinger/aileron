// Package draft orchestrates cloud-hosted draft generation.
//
// The Pipeline assembles context from source connectors, calls an LLM with
// tool access, and returns a draft reply. It bridges the source connector
// layer (read-only context retrieval), the LLM client (text generation with
// tool use), and the vault (credential management).
package draft

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/llm"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/source"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
)

// Pipeline orchestrates draft generation for incoming messages.
type Pipeline struct {
	llm               llm.Client
	sourceRegistry    *source.Registry
	connectedAccounts store.ConnectedAccountStore
	instructions      store.UserInstructionStore
	vault             vault.Vault
	log               *slog.Logger
	systemPrompt      string
}

// NewPipeline creates a draft generation pipeline. The systemPrompt is the
// base prompt loaded from AILERON.md (or the hardcoded default).
func NewPipeline(
	llmClient llm.Client,
	sourceReg *source.Registry,
	accounts store.ConnectedAccountStore,
	instructions store.UserInstructionStore,
	v vault.Vault,
	log *slog.Logger,
	systemPrompt string,
) *Pipeline {
	return &Pipeline{
		llm:               llmClient,
		sourceRegistry:    sourceReg,
		connectedAccounts: accounts,
		instructions:      instructions,
		vault:             v,
		log:               log,
		systemPrompt:      systemPrompt,
	}
}

// GenerateDraft produces a draft reply for an incoming message.
// It assembles context from source connectors available to the user,
// calls the LLM with tool access, and returns the draft text.
func (p *Pipeline) GenerateDraft(ctx context.Context, userID string, msg comms.IncomingMessage) (string, error) {
	// Assemble the system prompt from base + user instructions.
	prompt, err := p.assembleSystemPrompt(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("assembling system prompt: %w", err)
	}

	// Present the message as context — don't say "draft a reply" as that
	// triggers assistant-mode behavior. The system prompt already establishes
	// the identity and role.
	userMessage := fmt.Sprintf(
		"Message from %s in %s:\n\n%s",
		msg.Author, msg.Channel, msg.Body,
	)

	// Get the user's connected accounts to determine available tools.
	accounts, err := p.connectedAccounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
	if err != nil {
		return "", fmt.Errorf("listing connected accounts: %w", err)
	}

	// Collect tools from source connectors matching connected accounts.
	connectedProviders := make(map[string]bool)
	for _, acct := range accounts {
		if acct.Status == model.ConnectedAccountStatusActive {
			connectedProviders[string(acct.Provider)] = true
		}
	}

	var tools []source.ToolDefinition
	if p.sourceRegistry != nil {
		for _, provider := range p.sourceRegistry.Providers() {
			if connectedProviders[provider] {
				tools = append(tools, p.sourceRegistry.ToolsForProvider(provider)...)
			}
		}
	}

	// Build a tool executor that resolves credentials and calls source connectors.
	executor := p.buildToolExecutor(ctx, userID, accounts)

	resp, err := p.llm.GenerateWithTools(ctx, llm.GenerateRequest{
		SystemPrompt: prompt,
		UserMessage:  userMessage,
		Tools:        tools,
		ToolExecutor: executor,
	})
	if err != nil {
		return "", fmt.Errorf("LLM generation failed: %w", err)
	}

	p.log.Info("draft generated",
		"user_id", userID,
		"channel", msg.Channel,
		"tool_calls", len(resp.ToolCalls),
		"draft_length", len(resp.Text),
	)

	return resp.Text, nil
}

// assembleSystemPrompt builds the system prompt from the base prompt plus
// the user's active instructions. Instructions are the highest-priority
// context — they override learned patterns and behavioral model inferences.
func (p *Pipeline) assembleSystemPrompt(ctx context.Context, userID string) (string, error) {
	prompt := p.systemPrompt

	if p.instructions == nil {
		return prompt, nil
	}

	active := true
	instructions, err := p.instructions.List(ctx, store.UserInstructionFilter{
		UserID: userID,
		Active: &active,
	})
	if err != nil {
		return "", fmt.Errorf("listing instructions: %w", err)
	}

	if len(instructions) == 0 {
		return prompt, nil
	}

	prompt += "\n\n## User Instructions\n\nThe user has set these explicit rules. Follow them precisely:\n"
	for _, ins := range instructions {
		prompt += "\n- " + ins.Body
		if ins.Scope != "" {
			prompt += " [scope: " + ins.Scope + "]"
		}
	}

	return prompt, nil
}

// buildToolExecutor creates a closure that resolves credentials from the vault
// and dispatches tool calls to the source connector registry.
func (p *Pipeline) buildToolExecutor(ctx context.Context, userID string, accounts []model.ConnectedAccount) llm.ToolExecutor {
	return func(execCtx context.Context, tool string, params map[string]any) (map[string]any, error) {
		if p.sourceRegistry == nil {
			return nil, fmt.Errorf("no source registry configured")
		}

		// Resolve the tool to a provider.
		provider, err := p.sourceRegistry.ResolveToolProvider(tool)
		if err != nil {
			return nil, err
		}

		// Find the user's connected account for this provider.
		var acct *model.ConnectedAccount
		providerEnum := model.ConnectedAccountProvider(provider)
		for i := range accounts {
			if accounts[i].Provider == providerEnum && accounts[i].Status == model.ConnectedAccountStatusActive {
				acct = &accounts[i]
				break
			}
		}
		if acct == nil {
			return nil, fmt.Errorf("no connected %s account for user %s", provider, userID)
		}

		// Get the OAuth token from the vault.
		secret, err := p.vault.Get(ctx, acct.VaultPath())
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve %s credentials: %w", provider, err)
		}

		p.log.Debug("executing tool", "tool", tool, "provider", provider, "user_id", userID)
		return p.sourceRegistry.ExecuteTool(execCtx, tool, params, secret.Value)
	}
}
