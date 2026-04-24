package comms

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
)

// SendSlackMessage sends a message to a Slack channel using a one-shot client.
// Unlike SlackListener.Send() which requires a persistent Socket Mode connection,
// this creates a fresh client from the provided OAuth token, posts the message,
// and discards the client. Suitable for cloud-mode where no persistent Slack
// connection exists. When threadTS is non-empty the message is posted as a
// threaded reply.
func SendSlackMessage(ctx context.Context, token, channel, body, threadTS string) error {
	if token == "" {
		return fmt.Errorf("slack: token is required")
	}
	if channel == "" {
		return fmt.Errorf("slack: channel is required")
	}

	opts := []slack.MsgOption{
		slack.MsgOptionText(body, false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	client := slack.New(token)
	_, _, err := client.PostMessageContext(ctx, channel, opts...)
	return err
}
