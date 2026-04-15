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
// connection exists.
func SendSlackMessage(ctx context.Context, token, channel, body string) error {
	if token == "" {
		return fmt.Errorf("slack: token is required")
	}
	if channel == "" {
		return fmt.Errorf("slack: channel is required")
	}

	client := slack.New(token)
	_, _, err := client.PostMessageContext(ctx, channel,
		slack.MsgOptionText(body, false),
	)
	return err
}
