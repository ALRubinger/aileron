package app

import (
	"context"

	"github.com/ALRubinger/aileron/core/comms"
)

// defaultSlackAgentClient delegates to the package-level functions in core/comms.
type defaultSlackAgentClient struct{}

func (defaultSlackAgentClient) SetStatus(ctx context.Context, botToken, channelID, threadTS, status string) error {
	return comms.SetAssistantStatus(ctx, botToken, channelID, threadTS, status)
}

func (defaultSlackAgentClient) SetSuggestedPrompts(ctx context.Context, botToken, channelID, threadTS string, prompts []comms.SlackAgentPrompt) error {
	return comms.SetAssistantSuggestedPrompts(ctx, botToken, channelID, threadTS, prompts)
}

func (defaultSlackAgentClient) SetTitle(ctx context.Context, botToken, channelID, threadTS, title string) error {
	return comms.SetAssistantTitle(ctx, botToken, channelID, threadTS, title)
}

func (defaultSlackAgentClient) StartStream(ctx context.Context, botToken, channelID, threadTS string) (string, error) {
	return comms.StartStream(ctx, botToken, channelID, threadTS)
}

func (defaultSlackAgentClient) AppendStream(ctx context.Context, botToken, channelID, ts, text string) error {
	return comms.AppendStream(ctx, botToken, channelID, ts, text)
}

func (defaultSlackAgentClient) StopStream(ctx context.Context, botToken, channelID, ts string) error {
	return comms.StopStream(ctx, botToken, channelID, ts)
}

func (defaultSlackAgentClient) PostMessage(ctx context.Context, botToken, channelID, threadTS, text string) error {
	return comms.PostMessage(ctx, botToken, channelID, threadTS, text)
}
