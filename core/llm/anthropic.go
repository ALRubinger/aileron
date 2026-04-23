package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/core/source"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"golang.org/x/sync/errgroup"
)

const defaultMaxToolRounds = 15

// AnthropicClient implements Client using the Anthropic Messages API.
type AnthropicClient struct {
	client *anthropic.Client
	model  anthropic.Model
	log    *slog.Logger
}

// NewAnthropicClient creates an Anthropic LLM client.
func NewAnthropicClient(apiKey, model string, opts ...AnthropicOption) *AnthropicClient {
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	ac := &AnthropicClient{
		client: &c,
		model:  anthropic.Model(model),
		log:    slog.Default(),
	}
	for _, opt := range opts {
		opt(ac)
	}
	return ac
}

// AnthropicOption configures an AnthropicClient.
type AnthropicOption func(*AnthropicClient)

// WithLogger sets the logger for the AnthropicClient.
func WithLogger(log *slog.Logger) AnthropicOption {
	return func(c *AnthropicClient) {
		c.log = log
	}
}

// SetBaseURL overrides the Anthropic API base URL. Used for testing with
// a mock HTTP server.
func (a *AnthropicClient) SetBaseURL(url string) {
	c := anthropic.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(url),
	)
	a.client = &c
}

func (a *AnthropicClient) GenerateWithTools(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	totalStart := time.Now()
	maxRounds := req.MaxToolRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxToolRounds
	}

	tools := ConvertTools(req.Tools)

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(req.UserMessage)),
	}

	var allToolCalls []ToolCall

	for round := 0; round <= maxRounds; round++ {
		params := anthropic.MessageNewParams{
			Model:     a.model,
			MaxTokens: 4096,
			Messages:  messages,
		}
		if req.SystemPrompt != "" {
			params.System = []anthropic.TextBlockParam{
				{Text: req.SystemPrompt},
			}
		}
		if len(tools) > 0 {
			params.Tools = tools
		}

		apiStart := time.Now()
		resp, err := a.client.Messages.New(ctx, params)
		apiDur := time.Since(apiStart)
		if err != nil {
			return nil, fmt.Errorf("anthropic API error: %w", err)
		}
		a.log.Info("llm api call",
			"round", round,
			"model", string(a.model),
			"duration_ms", apiDur.Milliseconds(),
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
		)

		// Check if the response contains tool use requests.
		var toolUseBlocks []anthropic.ContentBlockUnion
		var textParts []string

		for _, block := range resp.Content {
			switch block.Type {
			case "tool_use":
				toolUseBlocks = append(toolUseBlocks, block)
			case "text":
				if block.Text != "" {
					textParts = append(textParts, block.Text)
				}
			}
		}

		// If no tool use, return the text response.
		if len(toolUseBlocks) == 0 || resp.StopReason == "end_turn" {
			text := ""
			for _, t := range textParts {
				if text != "" {
					text += "\n"
				}
				text += t
			}
			a.log.Info("llm generation complete",
				"model", string(a.model),
				"rounds", round,
				"tool_calls", len(allToolCalls),
				"total_ms", time.Since(totalStart).Milliseconds(),
			)
			return &GenerateResponse{
				Text:      text,
				ToolCalls: allToolCalls,
			}, nil
		}

		// Process tool calls.
		if req.ToolExecutor == nil {
			return nil, fmt.Errorf("LLM requested tool use but no ToolExecutor provided")
		}

		// Build assistant message with the full response content.
		var assistantBlocks []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			switch block.Type {
			case "text":
				assistantBlocks = append(assistantBlocks, anthropic.NewTextBlock(block.Text))
			case "tool_use":
				tu := block.AsToolUse()
				assistantBlocks = append(assistantBlocks, anthropic.NewToolUseBlock(tu.ID, tu.Input, tu.Name))
			}
		}
		messages = append(messages, anthropic.MessageParam{
			Role:    "assistant",
			Content: assistantBlocks,
		})

		// Execute tool calls in parallel and collect results.
		type toolExecResult struct {
			id       string
			toolCall ToolCall
			result   map[string]any
			err      error
		}
		execResults := make([]toolExecResult, len(toolUseBlocks))

		toolStart := time.Now()
		g, gCtx := errgroup.WithContext(ctx)
		var mu sync.Mutex
		for i, block := range toolUseBlocks {
			tu := block.AsToolUse()

			var params map[string]any
			if err := json.Unmarshal(tu.Input, &params); err != nil {
				params = make(map[string]any)
			}

			execResults[i].id = tu.ID
			execResults[i].toolCall = ToolCall{Tool: tu.Name, Params: params}

			g.Go(func() error {
				result, execErr := req.ToolExecutor(gCtx, tu.Name, params)
				mu.Lock()
				execResults[i] = toolExecResult{
					id:       tu.ID,
					toolCall: ToolCall{Tool: tu.Name, Params: params},
					result:   result,
					err:      execErr,
				}
				mu.Unlock()
				return nil // don't cancel other tools on individual error
			})
		}
		g.Wait()
		a.log.Info("tool execution complete",
			"round", round,
			"tools", len(toolUseBlocks),
			"duration_ms", time.Since(toolStart).Milliseconds(),
		)

		// Build tool results in original order.
		var toolResults []anthropic.ContentBlockParamUnion
		for _, er := range execResults {
			allToolCalls = append(allToolCalls, er.toolCall)
			if er.err != nil {
				errMsg := fmt.Sprintf("Error: %s", er.err.Error())
				a.log.Debug("tool result error", "tool", er.toolCall.Tool, "error", er.err)
				toolResults = append(toolResults, anthropic.NewToolResultBlock(er.id, errMsg, true))
				continue
			}
			resultJSON, _ := json.Marshal(er.result)
			a.log.Debug("tool result ok", "tool", er.toolCall.Tool, "result_bytes", len(resultJSON), "params", er.toolCall.Params)
			toolResults = append(toolResults, anthropic.NewToolResultBlock(er.id, string(resultJSON), false))
		}

		messages = append(messages, anthropic.MessageParam{
			Role:    "user",
			Content: toolResults,
		})
	}

	// Tool rounds exhausted. Make one final call WITHOUT tools to force
	// the LLM to produce a text response with whatever context it has
	// gathered so far. This prevents silent failures where the user gets
	// no draft at all.
	a.log.Warn("tool rounds exhausted, forcing text output",
		"model", string(a.model),
		"max_rounds", maxRounds,
		"tool_calls", len(allToolCalls),
		"elapsed_ms", time.Since(totalStart).Milliseconds(),
	)
	finalParams := anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: 4096,
		Messages:  messages,
	}
	if req.SystemPrompt != "" {
		finalParams.System = []anthropic.TextBlockParam{
			{Text: req.SystemPrompt},
		}
	}
	// No tools — force text output.
	resp, err := a.client.Messages.New(ctx, finalParams)
	if err != nil {
		return nil, fmt.Errorf("anthropic API error on final round: %w", err)
	}

	var textParts []string
	for _, block := range resp.Content {
		if block.Type == "text" && block.Text != "" {
			textParts = append(textParts, block.Text)
		}
	}
	text := ""
	for _, t := range textParts {
		if text != "" {
			text += "\n"
		}
		text += t
	}
	a.log.Info("llm generation complete (after exhaustion fallback)",
		"model", string(a.model),
		"rounds", maxRounds+1,
		"tool_calls", len(allToolCalls),
		"total_ms", time.Since(totalStart).Milliseconds(),
	)
	return &GenerateResponse{
		Text:      text,
		ToolCalls: allToolCalls,
	}, nil
}

// GenerateStream implements StreamingClient. It sends a prompt and streams
// text chunks back as they arrive from the Anthropic API. This is used for
// the ghostwrite round (no tools, text-only output).
func (a *AnthropicClient) GenerateStream(ctx context.Context, req GenerateRequest) (<-chan string, <-chan error) {
	textCh := make(chan string, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(textCh)
		defer close(errCh)

		params := anthropic.MessageNewParams{
			Model:     a.model,
			MaxTokens: 4096,
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(req.UserMessage)),
			},
		}
		if req.SystemPrompt != "" {
			params.System = []anthropic.TextBlockParam{
				{Text: req.SystemPrompt},
			}
		}

		stream := a.client.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		for stream.Next() {
			event := stream.Current()
			if event.Type != "content_block_delta" {
				continue
			}
			delta := event.AsContentBlockDelta()
			if delta.Delta.Type != "text_delta" {
				continue
			}
			text := delta.Delta.AsTextDelta().Text
			if text == "" {
				continue
			}
			select {
			case textCh <- text:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}

		if err := stream.Err(); err != nil {
			errCh <- fmt.Errorf("anthropic streaming error: %w", err)
		}
	}()

	return textCh, errCh
}

// ConvertTools converts source.ToolDefinition to Anthropic ToolParam format.
// Exported for testing schema structure.
func ConvertTools(tools []source.ToolDefinition) []anthropic.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}

	result := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		properties := make(map[string]any)
		var required []string

		for _, p := range t.Parameters {
			prop := map[string]any{
				"type":        p.Type,
				"description": p.Description,
			}
			properties[p.Name] = prop
			if p.Required {
				required = append(required, p.Name)
			}
		}

		schema := anthropic.ToolInputSchemaParam{
			Properties: properties,
			Required:   required,
		}

		result = append(result, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: schema,
			},
		})
	}

	return result
}
