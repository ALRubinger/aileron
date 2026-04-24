package comms

import (
	"context"
	"fmt"
	"sync"

	"github.com/slack-go/slack"
)

// slackAgentAPIURL overrides the Slack API URL for testing.
var (
	slackAgentAPIURL string
	slackAgentMu     sync.RWMutex
)

// SetAgentAPIURL sets the Slack API URL for agent functions. Call with empty string to reset.
func SetAgentAPIURL(url string) {
	slackAgentMu.Lock()
	slackAgentAPIURL = url
	slackAgentMu.Unlock()
}

// newAgentClient creates a one-shot Slack client for agent operations.
func newAgentClient(botToken string) *slack.Client {
	slackAgentMu.RLock()
	apiURL := slackAgentAPIURL
	slackAgentMu.RUnlock()

	var opts []slack.Option
	if apiURL != "" {
		opts = append(opts, slack.OptionAPIURL(apiURL))
	}
	return slack.New(botToken, opts...)
}

// SlackAgentPrompt is a suggested prompt shown when a user opens the Aileron agent.
type SlackAgentPrompt struct {
	Title   string
	Message string
}

// SetAssistantStatus sets the thinking/loading status on an agent thread.
// Pass an empty status to clear it.
func SetAssistantStatus(ctx context.Context, botToken, channelID, threadTS, status string) error {
	if botToken == "" {
		return fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	return client.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channelID,
		ThreadTS:  threadTS,
		Status:    status,
	})
}

// SetAssistantSuggestedPrompts sets the suggested prompts for an agent thread.
func SetAssistantSuggestedPrompts(ctx context.Context, botToken, channelID, threadTS string, prompts []SlackAgentPrompt) error {
	if botToken == "" {
		return fmt.Errorf("slack agent: bot token is required")
	}
	params := slack.AssistantThreadsSetSuggestedPromptsParameters{
		ChannelID: channelID,
		ThreadTS:  threadTS,
	}
	for _, p := range prompts {
		params.AddPrompt(p.Title, p.Message)
	}
	client := newAgentClient(botToken)
	return client.SetAssistantThreadsSuggestedPromptsContext(ctx, params)
}

// SetAssistantTitle sets the title of an agent thread.
func SetAssistantTitle(ctx context.Context, botToken, channelID, threadTS, title string) error {
	if botToken == "" {
		return fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	return client.SetAssistantThreadsTitleContext(ctx, slack.AssistantThreadsSetTitleParameters{
		ChannelID: channelID,
		ThreadTS:  threadTS,
		Title:     title,
	})
}

// StartStream starts a streaming message in a Slack channel thread.
// Returns the timestamp of the new streaming message.
func StartStream(ctx context.Context, botToken, channelID, threadTS string) (string, error) {
	if botToken == "" {
		return "", fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	var opts []slack.MsgOption
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := client.StartStreamContext(ctx, channelID, opts...)
	return ts, err
}

// AppendStream appends text to an in-progress streaming message.
func AppendStream(ctx context.Context, botToken, channelID, ts, text string) error {
	if botToken == "" {
		return fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	_, _, err := client.AppendStreamContext(ctx, channelID, ts, slack.MsgOptionText(text, false))
	return err
}

// StopStream finalizes a streaming message. Additional options (e.g. Block Kit
// blocks for action buttons) can be appended via opts.
func StopStream(ctx context.Context, botToken, channelID, ts string, opts ...slack.MsgOption) error {
	if botToken == "" {
		return fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	_, _, err := client.StopStreamContext(ctx, channelID, ts, opts...)
	return err
}

// OpenModal opens a Slack modal view using the provided trigger ID.
// Returns the view response including the view ID for subsequent updates.
func OpenModal(ctx context.Context, botToken, triggerID string, view slack.ModalViewRequest) (*slack.ViewResponse, error) {
	if botToken == "" {
		return nil, fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	return client.OpenViewContext(ctx, triggerID, view)
}

// UpdateModal updates an existing Slack modal view by view ID.
func UpdateModal(ctx context.Context, botToken, viewID string, view slack.ModalViewRequest) (*slack.ViewResponse, error) {
	if botToken == "" {
		return nil, fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	return client.UpdateViewContext(ctx, view, "", "", viewID)
}

// EmptyModalView returns a minimal ModalViewRequest for testing.
func EmptyModalView() slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: slack.NewTextBlockObject(slack.PlainTextType, "Test", false, false),
	}
}

// PostMessage sends a text message to a Slack channel thread.
func PostMessage(ctx context.Context, botToken, channelID, threadTS, text string) error {
	if botToken == "" {
		return fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := client.PostMessageContext(ctx, channelID, opts...)
	return err
}

// OpenConversation opens or resumes a DM with the given user.
// Returns the channel ID of the DM.
func OpenConversation(ctx context.Context, botToken, userID string) (string, error) {
	if botToken == "" {
		return "", fmt.Errorf("slack agent: bot token is required")
	}
	client := newAgentClient(botToken)
	params := &slack.OpenConversationParameters{
		Users: []string{userID},
	}
	channel, _, _, err := client.OpenConversationContext(ctx, params)
	if err != nil {
		return "", err
	}
	return channel.ID, nil
}

// ResolveSlackDisplayName looks up a Slack user's display name via users.info.
// Returns the display name, or falls back to real_name, then the raw user ID.
func ResolveSlackDisplayName(ctx context.Context, botToken, slackUserID string) string {
	if botToken == "" || slackUserID == "" {
		return slackUserID
	}
	client := newAgentClient(botToken)
	user, err := client.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		return slackUserID
	}
	if user.Profile.DisplayName != "" {
		return user.Profile.DisplayName
	}
	if user.RealName != "" {
		return user.RealName
	}
	return slackUserID
}
