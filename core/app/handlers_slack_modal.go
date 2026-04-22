package app

import (
	"context"
	"encoding/json"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/slack-go/slack"
)

// DraftModalMeta is stored in the modal's private_metadata to carry context
// across the open → update → submit lifecycle.
type DraftModalMeta struct {
	TargetChannel   string `json:"channel"`
	TargetThreadTS  string `json:"thread_ts,omitempty"`
	OriginalMessage string `json:"original_message,omitempty"`
	UserID          string `json:"user_id"` // Slack user ID
}

const (
	draftModalCallbackID   = "aileron_draft_modal"
	draftInputBlockID      = "draft_input"
	draftInputActionID     = "draft_text"
	instructionBlockID     = "instruction_input"
	instructionActionID    = "instruction_text"
	maxModalTextLength     = 3000
)

// openDraftModal opens a modal in loading state. Must be called within 3s of
// the trigger_id. Returns the view response (including view ID) for updates.
func openDraftModal(ctx context.Context, botToken, triggerID string, meta DraftModalMeta) (*slack.ViewResponse, error) {
	metaJSON, _ := json.Marshal(meta)

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      draftModalCallbackID,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Draft with Aileron", false, false),
		PrivateMetadata: string(metaJSON),
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewSectionBlock(
					slack.NewTextBlockObject(slack.MarkdownType, ":hourglass_flowing_sand: *Researching...*", false, false),
					nil, nil,
				),
			},
		},
	}

	return comms.OpenModal(ctx, botToken, triggerID, view)
}

// updateDraftModalWithDraft updates the modal with an editable draft and
// an instructions field. Submit button is "Send".
func updateDraftModalWithDraft(ctx context.Context, botToken, viewID, draftText string, meta DraftModalMeta) error {
	metaJSON, _ := json.Marshal(meta)

	// Truncate if needed — Slack limits plain_text_input to 3000 chars.
	truncated := false
	if len(draftText) > maxModalTextLength {
		draftText = draftText[:maxModalTextLength]
		truncated = true
	}

	draftInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "Edit your draft...", false, false),
		draftInputActionID,
	)
	draftInput.Multiline = true
	draftInput.InitialValue = draftText

	instructionInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "e.g. Make it more concise", false, false),
		instructionActionID,
	)
	instructionInput.Multiline = false

	blocks := []slack.Block{
		slack.NewInputBlock(
			draftInputBlockID,
			slack.NewTextBlockObject(slack.PlainTextType, "Draft", false, false),
			nil,
			draftInput,
		),
	}

	if truncated {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "_Draft was truncated to fit Slack's limit._", false, false),
			nil, nil,
		))
	}

	instructionInputBlock := slack.NewInputBlock(
		instructionBlockID,
		slack.NewTextBlockObject(slack.PlainTextType, "Instructions (optional)", false, false),
		nil,
		instructionInput,
	)
	instructionInputBlock.Optional = true
	blocks = append(blocks, instructionInputBlock)

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      draftModalCallbackID,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Draft with Aileron", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Send", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		PrivateMetadata: string(metaJSON),
		Blocks: slack.Blocks{
			BlockSet: blocks,
		},
	}

	_, err := comms.UpdateModal(ctx, botToken, viewID, view)
	return err
}

// updateDraftModalError updates the modal to show an error message.
func updateDraftModalError(ctx context.Context, botToken, viewID, message string, meta DraftModalMeta) error {
	metaJSON, _ := json.Marshal(meta)

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      draftModalCallbackID,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Draft with Aileron", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Close", false, false),
		PrivateMetadata: string(metaJSON),
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewSectionBlock(
					slack.NewTextBlockObject(slack.MarkdownType, ":x: "+message, false, false),
					nil, nil,
				),
			},
		},
	}

	_, err := comms.UpdateModal(ctx, botToken, viewID, view)
	return err
}

// parseDraftModalMeta extracts the DraftModalMeta from a view's private_metadata.
func parseDraftModalMeta(raw string) (DraftModalMeta, error) {
	var meta DraftModalMeta
	err := json.Unmarshal([]byte(raw), &meta)
	return meta, err
}
