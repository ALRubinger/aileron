// Package draft orchestrates cloud-hosted draft generation.
//
// The Pipeline uses a multi-round approach:
//   Round 1 (research): LLM has tools, searches broadly, gathers context.
//     Its output is internal — never shown to anyone.
//   Round 1b (backward pass): If the message implies a time range and the
//     research output has date gaps, a targeted follow-up research call
//     fills them. This is deterministic — it fires based on what was found,
//     not what the LLM decided to do.
//   Round 2 (ghostwrite): LLM has NO tools, receives the gathered context,
//     writes the reply in the user's voice. No narration possible because
//     there's no research phase.
package draft

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/llm"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/source"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
)

// Pipeline orchestrates draft generation for incoming messages.
type Pipeline struct {
	researchLLM       llm.Client
	ghostwriteLLM     llm.Client
	sourceRegistry    *source.Registry
	connectedAccounts store.ConnectedAccountStore
	instructions      store.UserInstructionStore
	vault             vault.Vault
	log               *slog.Logger
	researchPrompt    string
	ghostwritePrompt  string
}

// NewPipeline creates a draft generation pipeline.
// researchLLM handles tool-use context gathering (fast/cheap model like Haiku).
// ghostwriteLLM handles synthesis — composing the reply (capable model like Sonnet).
func NewPipeline(
	researchLLM llm.Client,
	ghostwriteLLM llm.Client,
	sourceReg *source.Registry,
	accounts store.ConnectedAccountStore,
	instructions store.UserInstructionStore,
	v vault.Vault,
	log *slog.Logger,
	prompts Prompts,
) *Pipeline {
	return &Pipeline{
		researchLLM:       researchLLM,
		ghostwriteLLM:     ghostwriteLLM,
		sourceRegistry:    sourceReg,
		connectedAccounts: accounts,
		instructions:      instructions,
		vault:             v,
		log:               log,
		researchPrompt:    prompts.Research,
		ghostwritePrompt:  prompts.Ghostwrite,
	}
}

// GenerateDraft produces a draft reply using a two-round approach:
//   Round 1: Research — LLM uses tools to gather context. Output is internal.
//   Round 2: Ghostwrite — LLM writes the reply using the gathered context.
//     No tools, no narration.
func (p *Pipeline) GenerateDraft(ctx context.Context, userID string, msg comms.IncomingMessage) (string, error) {
	// Get available tools.
	accounts, err := p.connectedAccounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
	if err != nil {
		return "", fmt.Errorf("listing connected accounts: %w", err)
	}

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

	executor := p.buildToolExecutor(ctx, userID, accounts)

	// --- Round 1: Research ---
	// The LLM gathers context using tools. Its output is a structured
	// summary of what it found — never shown to the user.
	researchSysPrompt := strings.ReplaceAll(
		p.researchPrompt,
		"{{today}}",
		time.Now().Format("2006-01-02 (Monday)"),
	)
	researchMessage := fmt.Sprintf(
		"Find relevant context to help reply to this message from %s in %s:\n\n%s",
		msg.Author, msg.Channel, msg.Body,
	)

	now := time.Now()
	var gatheredContext string
	if len(tools) > 0 {
		researchResp, err := p.researchLLM.GenerateWithTools(ctx, llm.GenerateRequest{
			SystemPrompt: researchSysPrompt,
			UserMessage:  researchMessage,
			Tools:        tools,
			ToolExecutor: executor,
		})
		if err != nil {
			p.log.Error("research round failed", "user_id", userID, "error", err)
			gatheredContext = "(No additional context available — tool search failed.)"
		} else {
			gatheredContext = researchResp.Text
			p.log.Info("research round complete",
				"user_id", userID,
				"tool_calls", len(researchResp.ToolCalls),
				"context_length", len(gatheredContext),
			)

			// --- Round 1b: Backward pass ---
			// If the message implies a time range and the research output has
			// date gaps, replay the LLM's own search queries with explicit
			// after/before date filters for the uncovered window.
			gatheredContext = p.backwardPass(ctx, gatheredContext, msg.Body, now, researchResp.ToolCalls, executor, userID)
		}
	} else {
		gatheredContext = "(No source connectors available — replying with general knowledge only.)"
	}

	// --- Round 2: Ghostwrite ---
	// The LLM writes the reply using AILERON.md instructions + user
	// instructions + gathered context. NO tools — just text in, text out.
	// This eliminates narration because there's no research to narrate.
	ghostwritePrompt, err := p.assembleSystemPrompt(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("assembling system prompt: %w", err)
	}

	ghostwriteMessage := fmt.Sprintf(
		"Message from %s in %s:\n\n%s\n\n---\n\nContext gathered from connected sources:\n\n%s",
		msg.Author, msg.Channel, msg.Body, gatheredContext,
	)

	draftResp, err := p.ghostwriteLLM.GenerateWithTools(ctx, llm.GenerateRequest{
		SystemPrompt: ghostwritePrompt,
		UserMessage:  ghostwriteMessage,
		// No tools — force text-only output.
	})
	if err != nil {
		return "", fmt.Errorf("ghostwrite round failed: %w", err)
	}

	p.log.Info("draft generated",
		"user_id", userID,
		"channel", msg.Channel,
		"draft_length", len(draftResp.Text),
	)

	return draftResp.Text, nil
}

// assembleSystemPrompt builds the system prompt from the base prompt plus
// the user's active instructions. Instructions are the highest-priority
// context — they override learned patterns and behavioral model inferences.
func (p *Pipeline) assembleSystemPrompt(ctx context.Context, userID string) (string, error) {
	prompt := p.ghostwritePrompt

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

// backwardPass checks whether the research output covers the full time range
// implied by the message. If date gaps are found, it replays the LLM's own
// search queries from round 1 with after/before date filters injected for
// the uncovered window. The LLM chose the queries (what terms matter); the
// pipeline enforces the dates (mechanical constraint).
func (p *Pipeline) backwardPass(
	ctx context.Context,
	gatheredContext string,
	messageBody string,
	now time.Time,
	researchToolCalls []llm.ToolCall,
	executor llm.ToolExecutor,
	userID string,
) string {
	tr := DetectTimeRange(messageBody, now)
	if tr == nil {
		return gatheredContext
	}

	uncovered := FindUncoveredDays(gatheredContext, *tr)
	if len(uncovered) == 0 {
		p.log.Info("backward pass: full date coverage",
			"user_id", userID,
			"range", tr.Label,
		)
		return gatheredContext
	}

	// Only replay search-type tool calls (ones the LLM used for querying).
	replayable := filterReplayable(researchToolCalls)
	if len(replayable) == 0 {
		p.log.Info("backward pass: no replayable search calls from research round",
			"user_id", userID,
		)
		return gatheredContext
	}

	p.log.Info("backward pass: replaying searches with date filters",
		"user_id", userID,
		"range", tr.Label,
		"uncovered_days", len(uncovered),
		"replay_calls", len(replayable),
	)

	// Build the date window covering all uncovered days.
	after := uncovered[0].Format("2006-01-02")
	// before is the day after the last uncovered day (exclusive end).
	before := uncovered[len(uncovered)-1].AddDate(0, 0, 1).Format("2006-01-02")

	// Replay each search call with date filters injected.
	var results []string
	for _, tc := range replayable {
		params := make(map[string]any, len(tc.Params)+2)
		for k, v := range tc.Params {
			params[k] = v
		}
		params["after"] = after
		params["before"] = before

		result, err := executor(ctx, tc.Tool, params)
		if err != nil {
			p.log.Debug("backward pass tool error",
				"tool", tc.Tool,
				"error", err,
			)
			continue
		}

		resultJSON, _ := json.Marshal(result)
		results = append(results, fmt.Sprintf("[%s %s–%s]: %s", tc.Tool, after, before, string(resultJSON)))
	}

	if len(results) == 0 {
		p.log.Info("backward pass: no additional results",
			"user_id", userID,
		)
		return gatheredContext
	}

	p.log.Info("backward pass complete",
		"user_id", userID,
		"replayed", len(replayable),
		"results", len(results),
	)

	return gatheredContext + "\n\n---\n\nAdditional context (date-filtered replay for " + after + " to " + before + "):\n\n" + strings.Join(results, "\n\n")
}

// searchTools are the tool names that support date-filtered replay.
var searchTools = map[string]bool{
	"slack_search_messages": true,
	"slack_channel_history": true,
	"github_search_issues":  true,
	"gmail_search":          true,
	"calendar_events":       true,
}

// filterReplayable returns only the tool calls that are search-type tools
// worth replaying with date filters.
func filterReplayable(calls []llm.ToolCall) []llm.ToolCall {
	var out []llm.ToolCall
	for _, tc := range calls {
		if searchTools[tc.Tool] {
			out = append(out, tc)
		}
	}
	return out
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
