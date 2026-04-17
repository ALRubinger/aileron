package comms

import (
	"context"

	"github.com/slack-go/slack"
)

// slackThreadAPIURL overrides the Slack API URL for testing.
var slackThreadAPIURL string

// SetThreadAPIURL sets the Slack API URL for testing. Call with empty string to reset.
func SetThreadAPIURL(url string) {
	slackThreadAPIURL = url
}

// SlackThreadHasReplies checks if a Slack message has replies (is an active
// thread). Returns true if the thread has at least one reply beyond the
// parent message. Used to decide whether to post ephemeral drafts in-thread
// (thread exists) or at channel level (no thread yet — Slack silently drops
// ephemeral messages in threads with no replies).
func SlackThreadHasReplies(ctx context.Context, botToken, channel, messageTS string) bool {
	if botToken == "" || channel == "" || messageTS == "" {
		return false
	}

	var opts []slack.Option
	if slackThreadAPIURL != "" {
		opts = append(opts, slack.OptionAPIURL(slackThreadAPIURL))
	}
	client := slack.New(botToken, opts...)
	msgs, _, _, err := client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: messageTS,
		Limit:     2,
	})
	if err != nil {
		return false
	}

	return len(msgs) > 1
}
