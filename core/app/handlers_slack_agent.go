package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/draft"
	"github.com/ALRubinger/aileron/core/vault"
)

// handleAssistantThreadStarted is called when a user opens the Aileron agent DM.
// It sets suggested prompts and a thread title.
func (s *apiServer) handleAssistantThreadStarted(ctx context.Context, teamID, channelID, threadTS string) {
	botToken, err := s.resolveWorkspaceBotToken(ctx, teamID)
	if err != nil {
		s.log.Error("agent thread started: failed to resolve bot token", "team_id", teamID, "error", err)
		return
	}

	prompts := []comms.SlackAgentPrompt{
		{Title: "Draft a reply", Message: "Draft a reply to a message"},
		{Title: "Write a message", Message: "Write a message for me"},
		{Title: "What needs attention?", Message: "What needs my attention?"},
	}

	if err := s.slackAgentClient.SetSuggestedPrompts(ctx, botToken, channelID, threadTS, prompts); err != nil {
		s.log.Error("agent thread started: failed to set suggested prompts", "error", err)
	}

	if err := s.slackAgentClient.SetTitle(ctx, botToken, channelID, threadTS, "Aileron"); err != nil {
		s.log.Error("agent thread started: failed to set title", "error", err)
	}
}

// handleAssistantMessage is called when a user sends a message in an agent DM thread.
// It runs the draft pipeline with streaming and delivers the response.
func (s *apiServer) handleAssistantMessage(ctx context.Context, teamID, channelID, threadTS, slackUserID, text string) {
	botToken, err := s.resolveWorkspaceBotToken(ctx, teamID)
	if err != nil {
		s.log.Error("agent message: failed to resolve bot token", "team_id", teamID, "error", err)
		return
	}

	userID, err := s.resolveAileronUserBySlack(ctx, slackUserID, teamID)
	if err != nil {
		s.log.Error("agent message: failed to resolve user", "slack_user_id", slackUserID, "error", err)
		_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")
		return
	}

	if s.draftPipeline == nil {
		s.log.Warn("agent message: draft pipeline not configured")
		_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")
		return
	}

	// Resolve user's KEK for vault decryption.
	pipeline := s.draftPipeline
	if kek := s.getUserKEK(userID); kek != nil {
		pipeline = pipeline.WithVault(vault.NewUserScopedVault(s.vault, kek))
		defer zeroBytes(kek)
	} else if s.kekSessionCache != nil {
		s.log.Warn("agent message: vault locked for user", "user_id", userID)
		_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")
		return
	}

	// Set status to "Researching..."
	_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "Researching...")

	msg := comms.BuildIncomingMessage("", channelID, slackUserID, text)
	chunkCh, errCh := pipeline.GenerateDraftStream(ctx, userID, msg)

	var streamTS string
	var fullText strings.Builder

	for chunk := range chunkCh {
		switch {
		case chunk.Phase == "writing" && chunk.Text == "" && streamTS == "":
			// Phase transition to writing — start the stream.
			_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "Writing...")
			ts, err := s.slackAgentClient.StartStream(ctx, botToken, channelID, threadTS)
			if err != nil {
				s.log.Error("agent message: failed to start stream", "error", err)
				return
			}
			streamTS = ts

		case chunk.Text != "" && streamTS != "":
			// Append text to the stream.
			fullText.WriteString(chunk.Text)
			if err := s.slackAgentClient.AppendStream(ctx, botToken, channelID, streamTS, chunk.Text); err != nil {
				s.log.Error("agent message: failed to append stream", "error", err)
			}

		case chunk.Text != "" && streamTS == "":
			// Non-streaming fallback: accumulate text, we'll post at the end.
			fullText.WriteString(chunk.Text)
		}
	}

	// Check for pipeline errors.
	if err := <-errCh; err != nil {
		s.log.Error("agent message: pipeline error", "user_id", userID, "error", err)
		_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")
		return
	}

	// Stop the stream.
	if streamTS != "" {
		if err := s.slackAgentClient.StopStream(ctx, botToken, channelID, streamTS); err != nil {
			s.log.Error("agent message: failed to stop stream", "error", err)
		}
	}

	// Clear status.
	_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")

	s.log.Info("agent message: draft delivered",
		"user_id", userID,
		"channel", channelID,
		"draft_length", fullText.Len(),
	)
}

// handleMessageShortcut handles the "Draft reply with Aileron" message shortcut.
// It opens a modal immediately (within Slack's 3s trigger_id window), then
// generates a draft asynchronously and updates the modal with the result.
func (s *apiServer) handleMessageShortcut(ctx context.Context, payload slackInteractionPayload) {
	teamID := ""
	if payload.Team != nil {
		teamID = payload.Team.ID
	}

	botToken, err := s.resolveWorkspaceBotToken(ctx, teamID)
	if err != nil {
		s.log.Error("message shortcut: failed to resolve bot token", "error", err)
		return
	}

	channelID := ""
	channelName := ""
	if payload.Channel != nil {
		channelID = payload.Channel.ID
		channelName = payload.Channel.Name
	}

	messageText := ""
	messageTS := ""
	messageThreadTS := ""
	if payload.Message != nil {
		messageText = payload.Message.Text
		messageTS = payload.Message.TS
		messageThreadTS = payload.Message.ThreadTS
	}
	if messageThreadTS == "" {
		messageThreadTS = messageTS
	}

	meta := DraftModalMeta{
		TargetChannel:  channelID,
		TargetThreadTS: messageThreadTS,
		OriginalMessage: messageText,
		UserID:         payload.User.ID,
	}

	// Open the modal immediately — must be within 3s of trigger_id.
	viewResp, err := openDraftModal(ctx, botToken, payload.TriggerID, meta)
	if err != nil {
		s.log.Error("message shortcut: failed to open modal", "error", err)
		return
	}

	// Resolve Aileron user.
	userID, err := s.resolveAileronUserBySlack(ctx, payload.User.ID, teamID)
	if err != nil {
		s.log.Error("message shortcut: failed to resolve user", "error", err)
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Could not find your Aileron account.", meta)
		return
	}

	if s.draftPipeline == nil {
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Draft generation is not configured.", meta)
		return
	}

	// Resolve vault.
	pipeline := s.draftPipeline
	if kek := s.getUserKEK(userID); kek != nil {
		pipeline = pipeline.WithVault(vault.NewUserScopedVault(s.vault, kek))
		defer zeroBytes(kek)
	} else if s.kekSessionCache != nil {
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Your vault is locked. Please unlock it first.", meta)
		return
	}

	// Build message context.
	preview := messageText
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	msg := comms.BuildIncomingMessage(messageTS, channelID, "", fmt.Sprintf(
		"[Message from #%s]: %s", channelName, preview,
	))

	// Generate draft (non-streaming for modal — we update once when done).
	draftText, err := pipeline.GenerateDraft(ctx, userID, msg)
	if err != nil {
		s.log.Error("message shortcut: draft generation failed", "error", err)
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Draft generation failed. Please try again.", meta)
		return
	}

	// Update the modal with the draft.
	if err := updateDraftModalWithDraft(ctx, botToken, viewResp.ID, draftText, meta); err != nil {
		s.log.Error("message shortcut: failed to update modal", "error", err)
	}
}

// handleSlackAgentSend handles the send_draft_agent action from an agent DM.
// It posts the draft text to the target channel as the user.
func (s *apiServer) handleSlackAgentSend(ctx context.Context, userID, targetChannel, targetThreadTS, draftBody string) error {
	return s.sendDraftMessage(ctx, userID, targetChannel, draftBody, targetThreadTS)
}

// buildStreamingPipeline resolves the user's vault and returns a configured pipeline.
// Returns nil if the pipeline is not configured or the vault is locked.
func (s *apiServer) buildStreamingPipeline(userID string) *draft.Pipeline {
	if s.draftPipeline == nil {
		return nil
	}
	pipeline := s.draftPipeline
	if kek := s.getUserKEK(userID); kek != nil {
		pipeline = pipeline.WithVault(vault.NewUserScopedVault(s.vault, kek))
		// Note: kek is not zeroed here — caller must handle lifecycle.
	} else if s.kekSessionCache != nil {
		return nil
	}
	return pipeline
}
