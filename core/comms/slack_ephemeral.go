package comms

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
)

// SlackDraftMessage contains the data needed to post an ephemeral draft
// preview to a user in a Slack channel.
type SlackDraftMessage struct {
	BotToken string // bot token for posting ephemeral messages
	Channel  string // channel where the original message was
	UserID   string // Slack user ID to show the ephemeral message to
	DraftID  string // Aileron draft ID for button callback data
	Author   string // who sent the original message
	Body     string // the original message text
	Draft    string // the AI-generated draft reply
}

// PostEphemeralDraft posts an ephemeral message with the draft and
// approve/edit/discard buttons. Only the target user sees it.
func PostEphemeralDraft(ctx context.Context, msg SlackDraftMessage) error {
	if msg.BotToken == "" {
		return fmt.Errorf("slack: bot token is required for ephemeral messages")
	}

	client := slack.New(msg.BotToken)

	// Truncate draft for preview if very long.
	draftPreview := msg.Draft
	if len(draftPreview) > 500 {
		draftPreview = draftPreview[:497] + "..."
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("*Draft reply to %s:*\n\n%s", msg.Author, draftPreview),
				false, false),
			nil, nil,
		),
		slack.NewActionBlock("draft_actions",
			slack.NewButtonBlockElement("approve_draft", msg.DraftID,
				slack.NewTextBlockObject("plain_text", "Approve", false, false),
			).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement("edit_draft", msg.DraftID,
				slack.NewTextBlockObject("plain_text", "Edit", false, false),
			),
			slack.NewButtonBlockElement("discard_draft", msg.DraftID,
				slack.NewTextBlockObject("plain_text", "Discard", false, false),
			).WithStyle(slack.StyleDanger),
		),
	}

	_, err := client.PostEphemeralContext(ctx, msg.Channel, msg.UserID,
		slack.MsgOptionBlocks(blocks...),
	)
	return err
}
