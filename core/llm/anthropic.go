package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/core/source"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const defaultMaxToolRounds = 5

// AnthropicClient implements Client using the Anthropic Messages API.
type AnthropicClient struct {
	client *anthropic.Client
	model  anthropic.Model
}

// NewAnthropicClient creates an Anthropic LLM client.
func NewAnthropicClient(apiKey, model string) *AnthropicClient {
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	return &AnthropicClient{
		client: &c,
		model:  anthropic.Model(model),
	}
}

func (a *AnthropicClient) GenerateWithTools(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	maxRounds := req.MaxToolRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxToolRounds
	}

	tools := convertTools(req.Tools)

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

		resp, err := a.client.Messages.New(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("anthropic API error: %w", err)
		}

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

		// Execute each tool and build tool result messages.
		var toolResults []anthropic.ContentBlockParamUnion
		for _, block := range toolUseBlocks {
			tu := block.AsToolUse()

			var params map[string]any
			if err := json.Unmarshal(tu.Input, &params); err != nil {
				params = make(map[string]any)
			}

			allToolCalls = append(allToolCalls, ToolCall{
				Tool:   tu.Name,
				Params: params,
			})

			result, execErr := req.ToolExecutor(ctx, tu.Name, params)
			if execErr != nil {
				toolResults = append(toolResults, anthropic.NewToolResultBlock(tu.ID, fmt.Sprintf("Error: %s", execErr.Error()), true))
				continue
			}

			resultJSON, _ := json.Marshal(result)
			toolResults = append(toolResults, anthropic.NewToolResultBlock(tu.ID, string(resultJSON), false))
		}

		messages = append(messages, anthropic.MessageParam{
			Role:    "user",
			Content: toolResults,
		})
	}

	return nil, fmt.Errorf("exceeded maximum tool rounds (%d)", maxRounds)
}

// convertTools converts source.ToolDefinition to Anthropic ToolParam format.
func convertTools(tools []source.ToolDefinition) []anthropic.ToolUnionParam {
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
			Type: "object",
			Properties: map[string]any{
				"properties": properties,
				"required":   required,
				"type":       "object",
			},
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
