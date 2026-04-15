// Package slack implements a SourceConnector for Slack, providing read-only
// tools for retrieving channel history, thread replies, and message search.
package slack

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/core/source"
	slackapi "github.com/slack-go/slack"
)

// Connector implements source.SourceConnector for Slack.
type Connector struct {
	// apiURLOverride overrides the Slack API base URL for testing.
	apiURLOverride string
}

// New creates a new Slack source connector.
func New() *Connector {
	return &Connector{}
}

// WithAPIURL returns a copy with a custom Slack API URL (for testing).
func (c *Connector) WithAPIURL(url string) *Connector {
	cp := *c
	cp.apiURLOverride = url
	return &cp
}

func (c *Connector) Provider() string { return "slack" }

func (c *Connector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{
			Name:        "slack_channel_history",
			Description: "Get recent messages from a Slack channel. Returns messages with user IDs, text, and timestamps.",
			Parameters: []source.ToolParam{
				{Name: "channel", Type: "string", Description: "The channel ID (e.g. C0BACKEND)", Required: true},
				{Name: "limit", Type: "integer", Description: "Maximum number of messages to return (default 20, max 100)", Required: false},
			},
		},
		{
			Name:        "slack_thread_replies",
			Description: "Get replies in a Slack thread. Returns all messages in the thread including the parent.",
			Parameters: []source.ToolParam{
				{Name: "channel", Type: "string", Description: "The channel ID containing the thread", Required: true},
				{Name: "thread_ts", Type: "string", Description: "The timestamp of the parent message", Required: true},
			},
		},
		{
			Name:        "slack_search_messages",
			Description: "Search Slack messages across all channels the user has access to. Returns matching messages with channel, user, text, and timestamp.",
			Parameters: []source.ToolParam{
				{Name: "query", Type: "string", Description: "The search query", Required: true},
				{Name: "count", Type: "integer", Description: "Maximum number of results (default 10, max 50)", Required: false},
			},
		},
	}
}

func (c *Connector) Execute(ctx context.Context, tool string, params map[string]any, token []byte) (map[string]any, error) {
	// Parse the token — stored as JSON with access_token field.
	accessToken, err := extractAccessToken(token)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	client := c.newClient(accessToken)

	switch tool {
	case "slack_channel_history":
		return c.channelHistory(ctx, client, params)
	case "slack_thread_replies":
		return c.threadReplies(ctx, client, params)
	case "slack_search_messages":
		return c.searchMessages(ctx, client, params)
	default:
		return nil, fmt.Errorf("unknown tool: %s", tool)
	}
}

func (c *Connector) newClient(token string) *slackapi.Client {
	opts := []slackapi.Option{}
	if c.apiURLOverride != "" {
		opts = append(opts, slackapi.OptionAPIURL(c.apiURLOverride))
	}
	return slackapi.New(token, opts...)
}

func (c *Connector) channelHistory(_ context.Context, client *slackapi.Client, params map[string]any) (map[string]any, error) {
	channel, ok := params["channel"].(string)
	if !ok || channel == "" {
		return nil, fmt.Errorf("channel parameter is required")
	}

	limit := 20
	if l, ok := params["limit"]; ok {
		limit = toInt(l, 20)
	}
	if limit > 100 {
		limit = 100
	}

	resp, err := client.GetConversationHistory(&slackapi.GetConversationHistoryParameters{
		ChannelID: channel,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("slack API error: %w", err)
	}

	messages := make([]map[string]any, 0, len(resp.Messages))
	for _, msg := range resp.Messages {
		messages = append(messages, map[string]any{
			"user":      msg.User,
			"text":      msg.Text,
			"timestamp": msg.Timestamp,
		})
	}

	return map[string]any{"messages": messages}, nil
}

func (c *Connector) threadReplies(_ context.Context, client *slackapi.Client, params map[string]any) (map[string]any, error) {
	channel, ok := params["channel"].(string)
	if !ok || channel == "" {
		return nil, fmt.Errorf("channel parameter is required")
	}
	threadTS, ok := params["thread_ts"].(string)
	if !ok || threadTS == "" {
		return nil, fmt.Errorf("thread_ts parameter is required")
	}

	msgs, _, _, err := client.GetConversationReplies(&slackapi.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
	})
	if err != nil {
		return nil, fmt.Errorf("slack API error: %w", err)
	}

	messages := make([]map[string]any, 0, len(msgs))
	for _, msg := range msgs {
		messages = append(messages, map[string]any{
			"user":      msg.User,
			"text":      msg.Text,
			"timestamp": msg.Timestamp,
		})
	}

	return map[string]any{"messages": messages}, nil
}

func (c *Connector) searchMessages(_ context.Context, client *slackapi.Client, params map[string]any) (map[string]any, error) {
	query, ok := params["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}

	count := 10
	if c, ok := params["count"]; ok {
		count = toInt(c, 10)
	}
	if count > 50 {
		count = 50
	}

	resp, err := client.SearchMessages(query, slackapi.SearchParameters{
		Count: count,
	})
	if err != nil {
		return nil, fmt.Errorf("slack API error: %w", err)
	}

	messages := make([]map[string]any, 0, len(resp.Matches))
	for _, match := range resp.Matches {
		messages = append(messages, map[string]any{
			"channel":   match.Channel.ID,
			"user":      match.User,
			"text":      match.Text,
			"timestamp": match.Timestamp,
		})
	}

	return map[string]any{"messages": messages}, nil
}

// extractAccessToken parses the stored token JSON and returns the access_token.
func extractAccessToken(token []byte) (string, error) {
	var data map[string]string
	if err := json.Unmarshal(token, &data); err != nil {
		return "", fmt.Errorf("decoding token JSON: %w", err)
	}
	at, ok := data["access_token"]
	if !ok || at == "" {
		return "", fmt.Errorf("no access_token in token data")
	}
	return at, nil
}

// toInt converts an any value to int with a default.
func toInt(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return def
		}
		return int(i)
	default:
		return def
	}
}
