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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/llm"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/vault"
)

// SourceExecutor executes a source connector tool. When set on the pipeline,
// it is used instead of vault.Get() + local ExecuteTool. This enables
// enclave-based execution where credentials never leave the TEE.
type SourceExecutor func(ctx context.Context, tool string, params map[string]any, vaultPath string) (map[string]any, error)

// Pipeline orchestrates draft generation for incoming messages.
type Pipeline struct {
	researchLLM       llm.Client
	ghostwriteLLM     llm.Client
	clientResolver    *llm.ClientResolver // optional; overrides researchLLM/ghostwriteLLM per-user
	sourceRegistry    *source.Registry
	connectedAccounts store.ConnectedAccountStore
	instructions      store.UserInstructionStore
	vault             vault.Vault
	sourceExecutor    SourceExecutor // optional; enclave-based source tool execution
	log               *slog.Logger
	researchPrompt          string
	intentResolutionPrompt  string
	ghostwritePrompt        string
	clock             func() time.Time // defaults to time.Now; override in tests
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
		researchLLM:            researchLLM,
		ghostwriteLLM:          ghostwriteLLM,
		sourceRegistry:         sourceReg,
		connectedAccounts:      accounts,
		instructions:           instructions,
		vault:                  v,
		log:                    log,
		researchPrompt:         prompts.Research,
		intentResolutionPrompt: prompts.IntentResolution,
		ghostwritePrompt:       prompts.Ghostwrite,
		clock:                  time.Now,
	}
}

// WithClientResolver returns a copy of the pipeline that resolves per-user
// LLM clients at draft generation time. When set, the resolver's result
// overrides the default researchLLM and ghostwriteLLM clients.
func (p *Pipeline) WithClientResolver(r *llm.ClientResolver) *Pipeline {
	cp := *p
	cp.clientResolver = r
	return &cp
}

// WithClock returns a copy of the pipeline that uses the given clock function
// instead of time.Now. For testing only.
func (p *Pipeline) WithClock(clock func() time.Time) *Pipeline {
	cp := *p
	cp.clock = clock
	return &cp
}

// WithVault returns a copy of the pipeline that uses the given vault for
// credential retrieval. Used to scope the vault with per-user encryption
// (e.g. wrapping with UserScopedVault for KEK-based decryption).
func (p *Pipeline) WithVault(v vault.Vault) *Pipeline {
	cp := *p
	cp.vault = v
	return &cp
}

// WithSourceExecutor returns a copy of the pipeline that uses the given
// executor for source tool calls. When set, the pipeline delegates tool
// execution to the executor (typically the TEE enclave) instead of
// retrieving credentials from the vault and executing locally. This
// ensures credentials never leave the enclave.
func (p *Pipeline) WithSourceExecutor(e SourceExecutor) *Pipeline {
	cp := *p
	cp.sourceExecutor = e
	return &cp
}


// ResolveIntents determines what the user wants and returns structured intents.
//
// The flow is:
//  1. Research — LLM gathers context using tools (same as GenerateDraft).
//  2. Intent Resolution — LLM classifies the request and extracts parameters.
//  3. For informational intents: Ghostwrite round produces the text response.
//     For write actions: the structured intent is returned directly.
func (p *Pipeline) ResolveIntents(ctx context.Context, userID string, msg comms.IncomingMessage) ([]ResolvedIntent, error) {
	researchClient, synthesisClient := p.researchLLM, p.ghostwriteLLM
	if p.clientResolver != nil {
		var resolveErr error
		researchClient, synthesisClient, resolveErr = p.clientResolver.Resolve(ctx, userID)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolving LLM config: %w", resolveErr)
		}
	}

	// --- Research round (identical to GenerateDraft) ---
	gatheredContext, err := p.runResearch(ctx, userID, msg, researchClient)
	if err != nil {
		return nil, err
	}

	// --- Intent Resolution round ---
	if p.intentResolutionPrompt == "" {
		// No intent resolution prompt — fall back to legacy ghostwrite-only path.
		// Treat everything as informational.
		ghostwritePrompt, err := p.assembleSystemPrompt(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("assembling system prompt: %w", err)
		}
		ghostwriteMessage := fmt.Sprintf(
			"Message from %s in %s:\n\n%s\n\n---\n\nContext gathered from connected sources:\n\n%s",
			msg.Author, msg.Channel, msg.Body, gatheredContext,
		)
		draftResp, err := synthesisClient.GenerateWithTools(ctx, llm.GenerateRequest{
			SystemPrompt: ghostwritePrompt,
			UserMessage:  ghostwriteMessage,
		})
		if err != nil {
			return nil, fmt.Errorf("ghostwrite round failed: %w", err)
		}
		return []ResolvedIntent{{
			Type:         IntentTypeInformational,
			Summary:      draftResp.Text,
			ResponseText: draftResp.Text,
		}}, nil
	}

	intentSysPrompt := strings.ReplaceAll(
		p.intentResolutionPrompt,
		"{{today}}",
		p.clock().Format("2006-01-02 (Monday)"),
	)
	intentMessage := fmt.Sprintf(
		"Message from %s in %s:\n\n%s\n\n---\n\nContext gathered from connected sources:\n\n%s",
		msg.Author, msg.Channel, msg.Body, gatheredContext,
	)

	intentResp, err := researchClient.GenerateWithTools(ctx, llm.GenerateRequest{
		SystemPrompt: intentSysPrompt,
		UserMessage:  intentMessage,
	})
	if err != nil {
		return nil, fmt.Errorf("intent resolution failed: %w", err)
	}

	p.log.Info("intent resolution complete",
		"user_id", userID,
		"response_length", len(intentResp.Text),
	)

	// Parse the intent resolution response.
	intent, err := parseIntentResponse(intentResp.Text)
	if err != nil {
		p.log.Warn("intent resolution parse failed, falling back to informational",
			"user_id", userID,
			"error", err,
			"raw_response", intentResp.Text,
		)
		// Fall back to informational — run ghostwrite.
		intent = &ResolvedIntent{Type: IntentTypeInformational}
	}

	// For informational intents, run the ghostwrite round.
	if !intent.IsWriteAction() {
		ghostwritePrompt, err := p.assembleSystemPrompt(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("assembling system prompt: %w", err)
		}
		ghostwriteMessage := fmt.Sprintf(
			"Message from %s in %s:\n\n%s\n\n---\n\nContext gathered from connected sources:\n\n%s",
			msg.Author, msg.Channel, msg.Body, gatheredContext,
		)

		draftResp, err := synthesisClient.GenerateWithTools(ctx, llm.GenerateRequest{
			SystemPrompt: ghostwritePrompt,
			UserMessage:  ghostwriteMessage,
		})
		if err != nil {
			return nil, fmt.Errorf("ghostwrite round failed: %w", err)
		}
		intent.Summary = draftResp.Text
		intent.ResponseText = draftResp.Text
	}

	p.log.Info("intent resolved",
		"user_id", userID,
		"type", string(intent.Type),
		"is_write", intent.IsWriteAction(),
		"summary_length", len(intent.Summary),
	)

	return []ResolvedIntent{*intent}, nil
}

// runResearch executes the research round: LLM gathers context using tools.
// Returns the gathered context string. Extracted from GenerateDraft for reuse.
func (p *Pipeline) runResearch(ctx context.Context, userID string, msg comms.IncomingMessage, researchClient llm.Client) (string, error) {
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

	authTracker := &authErrorTracker{}
	executor := p.buildToolExecutor(ctx, userID, accounts, authTracker)
	now := p.clock()

	var gatheredContext string
	if len(tools) > 0 {
		researchSysPrompt := strings.ReplaceAll(
			p.researchPrompt,
			"{{today}}",
			now.Format("2006-01-02 (Monday)"),
		)
		researchMessage := fmt.Sprintf(
			"Find relevant context to help reply to this message from %s in %s:\n\n%s",
			msg.Author, msg.Channel, msg.Body,
		)
		researchResp, err := researchClient.GenerateWithTools(ctx, llm.GenerateRequest{
			SystemPrompt: researchSysPrompt,
			UserMessage:  researchMessage,
			Tools:        tools,
			ToolExecutor: executor,
		})
		if err != nil {
			if errors.Is(err, vault.ErrCredentialUnavailable) {
				return "", fmt.Errorf("research round: %w", err)
			}
			p.log.Error("research round failed", "user_id", userID, "error", err)
			gatheredContext = "(No additional context available — tool search failed.)"
		} else {
			gatheredContext = researchResp.Text
			gatheredContext = p.backwardPass(ctx, gatheredContext, msg.Body, now, researchResp.ToolCalls, executor, userID)
		}
	} else {
		gatheredContext = "(No source connectors available — replying with general knowledge only.)"
	}

	return gatheredContext, nil
}

// parseIntentResponse parses the JSON output from the intent resolution LLM call.
func parseIntentResponse(raw string) (*ResolvedIntent, error) {
	// Strip markdown code fences if the LLM wrapped the JSON.
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		// Remove first and last lines (the fences).
		if len(lines) >= 3 {
			text = strings.Join(lines[1:len(lines)-1], "\n")
		}
		text = strings.TrimSpace(text)
	}

	var parsed struct {
		Type string `json:"type"`

		// Email fields
		To       []model.Recipient `json:"to,omitempty"`
		CC       []model.Recipient `json:"cc,omitempty"`
		BCC      []model.Recipient `json:"bcc,omitempty"`
		Subject  string            `json:"subject,omitempty"`
		BodyText string            `json:"body_text,omitempty"`
		BodyHTML string            `json:"body_html,omitempty"`
		SendMode string            `json:"send_mode,omitempty"`
		ThreadRef string           `json:"thread_ref,omitempty"`

		// Calendar fields
		Title          string                  `json:"title,omitempty"`
		Description    string                  `json:"description,omitempty"`
		StartTime      string                  `json:"start_time,omitempty"`
		EndTime        string                  `json:"end_time,omitempty"`
		Timezone       string                  `json:"timezone,omitempty"`
		Location       string                  `json:"location,omitempty"`
		Attendees      []model.CalendarAttendee `json:"attendees,omitempty"`
		ConferenceType string                  `json:"conference_type,omitempty"`

		// GitHub issue fields
		Repository     string   `json:"repository,omitempty"`
		IssueTitle     string   `json:"issue_title,omitempty"`
		IssueBody      string   `json:"issue_body,omitempty"`
		IssueLabels    []string `json:"issue_labels,omitempty"`
		IssueAssignees []string `json:"issue_assignees,omitempty"`
		IssueNumber    string   `json:"issue_number,omitempty"`
		Body           string   `json:"body,omitempty"` // comment body or Slack message

		// Slack fields
		Channel string `json:"channel,omitempty"`
	}

	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, fmt.Errorf("parsing intent JSON: %w", err)
	}

	intentType := IntentType(parsed.Type)

	switch intentType {
	case IntentTypeInformational:
		return &ResolvedIntent{Type: IntentTypeInformational}, nil

	case IntentTypeEmailSend, IntentTypeEmailDraft:
		summary := fmt.Sprintf("Send email to %s: %s", formatRecipientNames(parsed.To), parsed.Subject)
		if intentType == IntentTypeEmailDraft {
			summary = fmt.Sprintf("Draft email to %s: %s", formatRecipientNames(parsed.To), parsed.Subject)
		}
		sendMode := parsed.SendMode
		if sendMode == "" {
			if intentType == IntentTypeEmailDraft {
				sendMode = "draft_only"
			} else {
				sendMode = "send_now"
			}
		}
		return &ResolvedIntent{
			Type:    intentType,
			Summary: summary,
			Action: &model.ActionIntent{
				Type:    string(intentType),
				Summary: summary,
				Domain: model.DomainAction{
					Email: &model.EmailAction{
						To:       parsed.To,
						CC:       parsed.CC,
						BCC:      parsed.BCC,
						Subject:  parsed.Subject,
						BodyText: parsed.BodyText,
						BodyHTML: parsed.BodyHTML,
						SendMode: sendMode,
						ThreadRef: parsed.ThreadRef,
					},
				},
			},
		}, nil

	case IntentTypeCalendarEventCreate:
		summary := fmt.Sprintf("Create calendar event: %s", parsed.Title)
		action := &model.ActionIntent{
			Type:    string(intentType),
			Summary: summary,
			Domain: model.DomainAction{
				Calendar: &model.CalendarAction{
					Title:          parsed.Title,
					Description:    parsed.Description,
					Timezone:       parsed.Timezone,
					Location:       parsed.Location,
					Attendees:      parsed.Attendees,
					ConferenceType: parsed.ConferenceType,
				},
			},
		}
		if parsed.StartTime != "" {
			t, err := time.Parse(time.RFC3339, parsed.StartTime)
			if err != nil {
				// Try without timezone.
				t, err = time.Parse("2006-01-02T15:04:05", parsed.StartTime)
			}
			if err == nil {
				action.Domain.Calendar.StartTime = &t
			}
		}
		if parsed.EndTime != "" {
			t, err := time.Parse(time.RFC3339, parsed.EndTime)
			if err != nil {
				t, err = time.Parse("2006-01-02T15:04:05", parsed.EndTime)
			}
			if err == nil {
				action.Domain.Calendar.EndTime = &t
			}
		}
		return &ResolvedIntent{
			Type:    intentType,
			Summary: summary,
			Action:  action,
		}, nil

	case IntentTypeGitIssueCreate:
		summary := fmt.Sprintf("Create issue in %s: %s", parsed.Repository, parsed.IssueTitle)
		return &ResolvedIntent{
			Type:    intentType,
			Summary: summary,
			Action: &model.ActionIntent{
				Type:    string(intentType),
				Summary: summary,
				Domain: model.DomainAction{
					Git: &model.GitAction{
						Repository:     parsed.Repository,
						IssueTitle:     parsed.IssueTitle,
						IssueBody:      parsed.IssueBody,
						IssueLabels:    parsed.IssueLabels,
						IssueAssignees: parsed.IssueAssignees,
					},
				},
			},
		}, nil

	case IntentTypeGitIssueComment:
		summary := fmt.Sprintf("Comment on %s#%s", parsed.Repository, parsed.IssueNumber)
		body := parsed.Body
		return &ResolvedIntent{
			Type:    intentType,
			Summary: summary,
			Action: &model.ActionIntent{
				Type:    string(intentType),
				Summary: summary,
				Metadata: map[string]any{
					"repository":   parsed.Repository,
					"issue_number": parsed.IssueNumber,
					"body":         body,
				},
			},
		}, nil

	case IntentTypeSlackSend:
		summary := "Send Slack message"
		body := parsed.Body
		return &ResolvedIntent{
			Type:    intentType,
			Summary: summary,
			Action: &model.ActionIntent{
				Type:    string(intentType),
				Summary: summary,
				Metadata: map[string]any{
					"channel": parsed.Channel,
					"body":    body,
				},
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown intent type: %q", parsed.Type)
	}
}

func formatRecipientNames(recipients []model.Recipient) string {
	if len(recipients) == 0 {
		return "(no recipients)"
	}
	var names []string
	for _, r := range recipients {
		if r.Name != "" {
			names = append(names, r.Name)
		} else {
			names = append(names, r.Email)
		}
	}
	return strings.Join(names, ", ")
}

// GenerateDraft produces a draft reply using a two-round approach:
//   Round 1: Research — LLM uses tools to gather context. Output is internal.
//   Round 2: Ghostwrite — LLM writes the reply using the gathered context.
//     No tools, no narration.
func (p *Pipeline) GenerateDraft(ctx context.Context, userID string, msg comms.IncomingMessage) (string, error) {
	// Resolve LLM clients: per-user/per-org config overrides defaults.
	researchClient, synthesisClient := p.researchLLM, p.ghostwriteLLM
	if p.clientResolver != nil {
		var resolveErr error
		researchClient, synthesisClient, resolveErr = p.clientResolver.Resolve(ctx, userID)
		if resolveErr != nil {
			return "", fmt.Errorf("resolving LLM config: %w", resolveErr)
		}
	}

	// Get available tools.
	accounts, err := p.connectedAccounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
	if err != nil {
		return "", fmt.Errorf("listing connected accounts: %w", err)
	}

	connectedProviders := make(map[string]bool)
	for _, acct := range accounts {
		if acct.Status == model.ConnectedAccountStatusActive {
			connectedProviders[string(acct.Provider)] = true
			p.log.Debug("connected account active", "provider", acct.Provider, "user_id", userID, "vault_path", acct.VaultPath())
		}
	}

	var registeredProviders []string
	var tools []source.ToolDefinition
	if p.sourceRegistry != nil {
		for _, provider := range p.sourceRegistry.Providers() {
			registeredProviders = append(registeredProviders, provider)
			if connectedProviders[provider] {
				providerTools := p.sourceRegistry.ToolsForProvider(provider)
				tools = append(tools, providerTools...)
				p.log.Debug("matched provider", "provider", provider, "tools", len(providerTools))
			} else {
				p.log.Debug("provider not connected", "provider", provider)
			}
		}
	}
	p.log.Debug("tool resolution",
		"user_id", userID,
		"connected_providers", fmt.Sprintf("%v", connectedProviders),
		"registered_providers", fmt.Sprintf("%v", registeredProviders),
		"total_tools", len(tools),
	)

	authTracker := &authErrorTracker{}
	executor := p.buildToolExecutor(ctx, userID, accounts, authTracker)

	// --- Round 1: Research ---
	// The LLM gathers context using tools. Its output is a structured
	// summary of what it found — never shown to the user.
	researchSysPrompt := strings.ReplaceAll(
		p.researchPrompt,
		"{{today}}",
		p.clock().Format("2006-01-02 (Monday)"),
	)
	researchMessage := fmt.Sprintf(
		"Find relevant context to help reply to this message from %s in %s:\n\n%s",
		msg.Author, msg.Channel, msg.Body,
	)

	now := p.clock()
	var gatheredContext string
	if len(tools) > 0 {
		researchResp, err := researchClient.GenerateWithTools(ctx, llm.GenerateRequest{
			SystemPrompt: researchSysPrompt,
			UserMessage:  researchMessage,
			Tools:        tools,
			ToolExecutor: executor,
		})
		if err != nil {
			if errors.Is(err, vault.ErrCredentialUnavailable) {
				return "", fmt.Errorf("research round: %w", err)
			}
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

	// If any providers had auth failures, propagate so the handler can
	// tell the user to re-authenticate — even if other providers succeeded.
	if failedProviders := authTracker.failed(); len(failedProviders) > 0 {
		p.log.Warn("providers with auth failures",
			"user_id", userID,
			"providers", failedProviders,
		)
		return "", fmt.Errorf("research round: %w", &source.AuthFailedError{
			Providers: dedupStrings(failedProviders),
		})
	}

	p.log.Debug("gathered context for ghostwriter",
		"user_id", userID,
		"context_length", len(gatheredContext),
		"context_preview", truncate(gatheredContext, 500),
	)

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
	p.log.Debug("ghostwrite input",
		"user_id", userID,
		"message_length", len(ghostwriteMessage),
		"system_prompt_length", len(ghostwritePrompt),
	)

	draftResp, err := synthesisClient.GenerateWithTools(ctx, llm.GenerateRequest{
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

// DraftChunk is a piece of a streaming draft response.
type DraftChunk struct {
	Text  string // incremental text delta
	Phase string // "researching" or "writing"
}

// GenerateDraftStream is like GenerateDraft but streams text chunks as
// the ghostwrite LLM produces them. The research rounds run synchronously
// (same as GenerateDraft). The returned channels are both closed when
// generation is complete; errCh receives at most one value.
func (p *Pipeline) GenerateDraftStream(ctx context.Context, userID string, msg comms.IncomingMessage) (<-chan DraftChunk, <-chan error) {
	chunkCh := make(chan DraftChunk, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		// Notify: research phase starting.
		select {
		case chunkCh <- DraftChunk{Phase: "researching"}:
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		}

		// --- Reuse research logic from GenerateDraft ---
		researchClient, synthesisClient := p.researchLLM, p.ghostwriteLLM
		if p.clientResolver != nil {
			var resolveErr error
			researchClient, synthesisClient, resolveErr = p.clientResolver.Resolve(ctx, userID)
			if resolveErr != nil {
				errCh <- fmt.Errorf("resolving LLM config: %w", resolveErr)
				return
			}
		}

		accounts, err := p.connectedAccounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
		if err != nil {
			errCh <- fmt.Errorf("listing connected accounts: %w", err)
			return
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

		authTracker := &authErrorTracker{}
		executor := p.buildToolExecutor(ctx, userID, accounts, authTracker)
		now := p.clock()

		var gatheredContext string
		if len(tools) > 0 {
			researchResp, err := researchClient.GenerateWithTools(ctx, llm.GenerateRequest{
				SystemPrompt: strings.ReplaceAll(p.researchPrompt, "{{today}}", now.Format("2006-01-02 (Monday)")),
				UserMessage: fmt.Sprintf(
					"Find relevant context to help reply to this message from %s in %s:\n\n%s",
					msg.Author, msg.Channel, msg.Body),
				Tools:        tools,
				ToolExecutor: executor,
			})
			if err != nil {
				if errors.Is(err, vault.ErrCredentialUnavailable) {
					errCh <- fmt.Errorf("research round: %w", err)
					return
				}
				p.log.Error("research round failed", "user_id", userID, "error", err)
				gatheredContext = "(No additional context available — tool search failed.)"
			} else {
				gatheredContext = researchResp.Text
				gatheredContext = p.backwardPass(ctx, gatheredContext, msg.Body, now, researchResp.ToolCalls, executor, userID)
			}
		} else {
			gatheredContext = "(No source connectors available — replying with general knowledge only.)"
		}

		// If any providers had auth failures, propagate so the handler can
		// tell the user to re-authenticate.
		if failedProviders := authTracker.failed(); len(failedProviders) > 0 {
			p.log.Warn("providers with auth failures",
				"user_id", userID,
				"providers", failedProviders,
			)
			errCh <- fmt.Errorf("research round: %w", &source.AuthFailedError{
				Providers: dedupStrings(failedProviders),
			})
			return
		}

		// --- Ghostwrite round: stream if possible ---
		ghostwritePrompt, err := p.assembleSystemPrompt(ctx, userID)
		if err != nil {
			errCh <- fmt.Errorf("assembling system prompt: %w", err)
			return
		}

		ghostwriteMessage := fmt.Sprintf(
			"Message from %s in %s:\n\n%s\n\n---\n\nContext gathered from connected sources:\n\n%s",
			msg.Author, msg.Channel, msg.Body, gatheredContext,
		)

		// Notify: writing phase starting.
		select {
		case chunkCh <- DraftChunk{Phase: "writing"}:
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		}

		// Try streaming if the ghostwrite client supports it.
		if sc, ok := synthesisClient.(llm.StreamingClient); ok {
			textCh, streamErrCh := sc.GenerateStream(ctx, llm.GenerateRequest{
				SystemPrompt: ghostwritePrompt,
				UserMessage:  ghostwriteMessage,
			})
			for text := range textCh {
				select {
				case chunkCh <- DraftChunk{Text: text, Phase: "writing"}:
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
			}
			if err := <-streamErrCh; err != nil {
				errCh <- err
			}
			return
		}

		// Fallback: non-streaming ghostwrite.
		draftResp, err := synthesisClient.GenerateWithTools(ctx, llm.GenerateRequest{
			SystemPrompt: ghostwritePrompt,
			UserMessage:  ghostwriteMessage,
		})
		if err != nil {
			errCh <- fmt.Errorf("ghostwrite round failed: %w", err)
			return
		}

		select {
		case chunkCh <- DraftChunk{Text: draftResp.Text, Phase: "writing"}:
		case <-ctx.Done():
			errCh <- ctx.Err()
		}
	}()

	return chunkCh, errCh
}

// RefineDraft revises a previously generated draft based on user feedback.
// It skips the research round (context was already gathered for the original
// draft) and runs only the ghostwrite round with the previous draft and
// feedback as additional context.
func (p *Pipeline) RefineDraft(ctx context.Context, userID string, originalMessage, previousDraft, feedback string) (string, error) {
	_, synthesisClient := p.researchLLM, p.ghostwriteLLM
	if p.clientResolver != nil {
		var resolveErr error
		_, synthesisClient, resolveErr = p.clientResolver.Resolve(ctx, userID)
		if resolveErr != nil {
			return "", fmt.Errorf("resolving LLM config: %w", resolveErr)
		}
	}

	ghostwritePrompt, err := p.assembleSystemPrompt(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("assembling system prompt: %w", err)
	}

	ghostwriteMessage := fmt.Sprintf(
		"Original message:\n\n%s\n\n---\n\nPrevious draft:\n\n%s\n\n---\n\nUser feedback:\n\n%s\n\nRevise the draft based on the feedback. Output only the revised draft — no commentary.",
		originalMessage, previousDraft, feedback,
	)

	resp, err := synthesisClient.GenerateWithTools(ctx, llm.GenerateRequest{
		SystemPrompt: ghostwritePrompt,
		UserMessage:  ghostwriteMessage,
	})
	if err != nil {
		return "", fmt.Errorf("refine draft failed: %w", err)
	}

	p.log.Info("draft refined", "user_id", userID, "draft_length", len(resp.Text))
	return resp.Text, nil
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
// authErrorTracker records providers that returned authentication errors
// during tool execution. This allows the research round to continue with
// working providers while still surfacing the auth failures afterward.
type authErrorTracker struct {
	mu        sync.Mutex
	providers []string // providers that returned ErrAuthFailed
}

func (t *authErrorTracker) record(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.providers = append(t.providers, provider)
}

func (t *authErrorTracker) failed() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.providers...)
}

func (p *Pipeline) buildToolExecutor(ctx context.Context, userID string, accounts []model.ConnectedAccount, authTracker *authErrorTracker) llm.ToolExecutor {
	return func(execCtx context.Context, tool string, params map[string]any) (map[string]any, error) {
		if p.sourceRegistry == nil && p.sourceExecutor == nil {
			return nil, fmt.Errorf("no source registry or executor configured")
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

		// Enclave execution path: delegate to the source executor so
		// credentials never leave the TEE. The executor handles credential
		// retrieval, token refresh, and escrow updates internally.
		if p.sourceExecutor != nil {
			result, execErr := p.sourceExecutor(execCtx, tool, params, acct.VaultPath())
			if execErr != nil {
				if errors.Is(execErr, source.ErrAuthFailed) {
					authTracker.record(provider)
					p.log.Warn("tool auth failed via enclave",
						"tool", tool, "provider", provider, "error", execErr)
				}
				p.log.Error("tool execution failed", "tool", tool, "provider", provider, "error", execErr)
			} else {
				resultSize := 0
				if result != nil {
					if b, err := json.Marshal(result); err == nil {
						resultSize = len(b)
					}
				}
				p.log.Debug("tool execution succeeded (enclave)", "tool", tool, "provider", provider, "result_bytes", resultSize)
			}
			return result, execErr
		}

		// Local execution path: retrieve credential from vault and execute
		// on the host. Used when vault is unlocked (KEK available).

		// Get the OAuth token from the vault. Any failure here is fatal —
		// the vault is either locked, the escrow is stale, or the KEK is
		// wrong. All subsequent tool calls for this provider will fail too,
		// so abort the LLM loop and let the handler tell the user directly.
		secret, err := p.vault.Get(ctx, acct.VaultPath())
		if err != nil {
			return nil, &llm.ToolFatalError{Err: fmt.Errorf(
				"%s: %w", err, vault.ErrCredentialUnavailable,
			)}
		}

		credLen := len(secret.Value)
		isEnc := vault.IsEncrypted(secret.Metadata)
		p.log.Debug("tool credential retrieved",
			"tool", tool,
			"provider", provider,
			"vault_path", acct.VaultPath(),
			"credential_bytes", credLen,
			"metadata_encrypted", isEnc,
		)

		// Inject a token saver so connectors can persist refreshed OAuth
		// tokens (including rotated refresh tokens) back to the vault.
		execCtx = source.WithTokenSaver(execCtx, func(saveCtx context.Context, newToken []byte) error {
			p.log.Info("persisting refreshed OAuth token",
				"provider", provider,
				"vault_path", acct.VaultPath(),
			)
			return p.vault.Put(saveCtx, acct.VaultPath(), newToken, secret.Metadata)
		})

		result, execErr := p.sourceRegistry.ExecuteTool(execCtx, tool, params, secret.Value)
		if execErr != nil {
			if errors.Is(execErr, source.ErrAuthFailed) {
				// Auth failure for this provider — record it so we can
				// surface it to the user after research completes, but
				// don't abort the loop. Other providers may still work.
				authTracker.record(provider)
				p.log.Warn("tool auth failed, continuing with other providers",
					"tool", tool, "provider", provider, "error", execErr)
			}
			p.log.Error("tool execution failed", "tool", tool, "provider", provider, "error", execErr)
		} else {
			resultSize := 0
			if result != nil {
				if b, err := json.Marshal(result); err == nil {
					resultSize = len(b)
				}
			}
			p.log.Debug("tool execution succeeded", "tool", tool, "provider", provider, "result_bytes", resultSize)
		}
		return result, execErr
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func dedupStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
