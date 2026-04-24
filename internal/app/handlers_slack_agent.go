package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/slack-go/slack"
)

// resolvePipelineVault returns the draft pipeline configured with the best
// available credential source for the given user:
//  1. KEK session (user recently unlocked vault) → UserScopedVault
//  2. Escrow (credentials escrowed in TEE) → EscrowVault
//  3. Active TEE session (vault unlocked, no escrow entries) → base pipeline
//  4. Neither available → returns nil
func (s *apiServer) resolvePipelineVault(userID string) *draft.Pipeline {
	if s.draftPipeline == nil {
		return nil
	}

	// Tier 1: KEK in session cache (vault unlocked).
	if kek := s.getUserKEK(userID); kek != nil {
		p := s.draftPipeline.WithVault(vault.NewUserScopedVault(s.vault, kek))
		// Note: kek is copied by UserScopedVault; zeroing deferred by caller
		// is not needed since the pipeline holds its own copy.
		return p
	}

	// Tier 2: Escrowed credentials in TEE.
	if s.enclaveClient != nil {
		// Check if this specific user has escrowed credentials.
		// Vault paths are "connected-accounts/{userID}/{provider}".
		prefix := "connected-accounts/" + userID + "/"
		hasEscrow := false
		s.escrowIndex.Range(func(key, _ any) bool {
			if path, ok := key.(string); ok && strings.HasPrefix(path, prefix) {
				hasEscrow = true
				return false // found one, stop
			}
			return true // keep looking
		})
		if hasEscrow {
			escrowVault := vault.NewEscrowVault(s.enclaveClient, &s.escrowIndex, s.vault)
			return s.draftPipeline.WithVault(escrowVault)
		}
	}

	// Tier 3: Active TEE session but no escrow entries (e.g. enclave
	// restarted, or user has no connected accounts with stored creds).
	// The vault IS unlocked — return the base pipeline so the user isn't
	// told to unlock when they already have.
	if s.teeState != nil {
		s.teeState.mu.Lock()
		teeExpiry, hasTeeSession := s.teeState.userSessions[userID]
		s.teeState.mu.Unlock()
		if hasTeeSession && time.Now().Before(teeExpiry) {
			return s.draftPipeline
		}
	}

	// Tier 4: Auth not enabled (local dev) — use base pipeline without vault.
	if s.kekSessionCache == nil {
		return s.draftPipeline
	}

	// No credentials available.
	return nil
}

// vaultUnlockURL returns the URL to the vault unlock page, falling back
// to the production URL when AILERON_UI_BASE_URL is not configured.
func (s *apiServer) vaultUnlockURL() string {
	base := s.uiBaseURL
	if base == "" {
		base = "https://app.withaileron.ai"
	}
	return base + "/vault"
}

// vaultLockedSlackMessage returns a Slack mrkdwn-formatted message with a
// clickable link to unlock the vault.
func (s *apiServer) vaultLockedSlackMessage() string {
	return ":lock: Your Aileron session has expired. <" + s.vaultUnlockURL() + "|Unlock your vault> to reconnect your accounts (takes 10 seconds, lasts 7 days)."
}

// isVaultError checks whether err indicates that vault credentials are
// unavailable (locked vault or stale escrow).
func isVaultError(err error) bool {
	return errors.Is(err, vault.ErrCredentialUnavailable)
}

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
		{Title: "Send an email", Message: "Send an email"},
		{Title: "Create a calendar invite", Message: "Create a calendar invite"},
		{Title: "File a GitHub issue", Message: "File a GitHub issue"},
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

	pipeline := s.resolvePipelineVault(userID)
	if pipeline == nil {
		if s.draftPipeline == nil {
			s.log.Warn("agent message: draft pipeline not configured")
		} else {
			s.log.Warn("agent message: vault locked for user", "user_id", userID)
			_ = s.slackAgentClient.PostMessage(ctx, botToken, channelID, threadTS, s.vaultLockedSlackMessage())
		}
		_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")
		return
	}

	// Set status to "Researching..."
	_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "Researching...")

	displayName := comms.ResolveSlackDisplayName(ctx, botToken, slackUserID)
	msg := comms.BuildIncomingMessage("", "Agent DM", displayName, text)
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
		if isVaultError(err) {
			_ = s.slackAgentClient.PostMessage(ctx, botToken, channelID, threadTS, s.vaultLockedSlackMessage())
		}
		_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")
		return
	}

	// Stop the stream.
	if streamTS != "" {
		if err := s.slackAgentClient.StopStream(ctx, botToken, channelID, streamTS); err != nil {
			s.log.Error("agent message: failed to stop stream", "error", err)
		}
	}

	// Post Send/Refine action buttons below the draft.
	if draftBody := fullText.String(); draftBody != "" {
		s.postDraftActionButtons(ctx, botToken, channelID, threadTS, userID, draftBody)
	}

	// Clear status.
	_ = s.slackAgentClient.SetStatus(ctx, botToken, channelID, threadTS, "")

	s.log.Info("agent message: draft delivered",
		"user_id", userID,
		"channel", channelID,
		"draft_length", fullText.Len(),
	)
}

// postDraftActionButtons posts a follow-up message with Send/Refine action buttons
// below a streamed draft in an agent DM. The Send button carries the draft body
// and target info so the interactions handler can send it as the user.
func (s *apiServer) postDraftActionButtons(ctx context.Context, botToken, channelID, threadTS, userID, draftBody string) {
	sendMeta, _ := json.Marshal(map[string]string{
		"channel":   channelID,
		"thread_ts": threadTS,
		"body":      draftBody,
		"user_id":   userID,
	})

	sendBtn := slack.NewButtonBlockElement("send_draft_agent", string(sendMeta),
		slack.NewTextBlockObject(slack.PlainTextType, "Send", true, false))
	sendBtn.Style = slack.StylePrimary

	cancelBtn := slack.NewButtonBlockElement("cancel_draft", "",
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false))
	cancelBtn.Style = slack.StyleDanger

	actionBlock := slack.NewActionBlock("draft_actions",
		sendBtn,
		cancelBtn,
	)

	client := comms.NewAgentClient(botToken)
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(actionBlock),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	if _, _, err := client.PostMessageContext(ctx, channelID, opts...); err != nil {
		s.log.Error("agent message: failed to post action buttons", "error", err)
	}
}

// handleMessageShortcut handles the "Draft reply with Aileron" message shortcut.
// It opens a modal prompting the user for drafting instructions, then generates
// a draft asynchronously when the user submits.
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
	if payload.Channel != nil {
		channelID = payload.Channel.ID
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
		TargetChannel:   channelID,
		TargetThreadTS:  messageThreadTS,
		OriginalMessage: messageText,
		UserID:          payload.User.ID,
		TeamID:          teamID,
	}

	// Open the instructions modal — must be within 3s of trigger_id.
	if _, err := openDraftInstructionsModal(ctx, botToken, payload.TriggerID, meta); err != nil {
		s.log.Error("message shortcut: failed to open instructions modal", "error", err)
	}
}

// generateDraftFromInstructions runs draft generation after the user submits
// instructions from the message shortcut modal. It updates the modal with the
// result (or an error).
func (s *apiServer) generateDraftFromInstructions(ctx context.Context, viewID string, meta DraftModalMeta) {
	botToken, err := s.resolveWorkspaceBotToken(ctx, meta.TeamID)
	if err != nil {
		s.log.Error("draft from instructions: failed to resolve bot token", "error", err)
		return
	}

	userID, err := s.resolveAileronUserBySlack(ctx, meta.UserID, meta.TeamID)
	if err != nil {
		s.log.Error("draft from instructions: failed to resolve user", "error", err)
		_ = updateDraftModalError(ctx, botToken, viewID, "Could not find your Aileron account.", meta)
		return
	}

	pipeline := s.resolvePipelineVault(userID)
	if pipeline == nil {
		if s.draftPipeline == nil {
			_ = updateDraftModalError(ctx, botToken, viewID, "Draft generation is not configured.", meta)
		} else {
			_ = updateDraftModalError(ctx, botToken, viewID, s.vaultLockedSlackMessage(), meta)
		}
		return
	}

	// Build message context with user instructions.
	preview := meta.OriginalMessage
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	messageContent := fmt.Sprintf("[Original message]: %s\n\n[User instructions]: %s", preview, meta.Instructions)
	msg := comms.BuildIncomingMessage("", meta.TargetChannel, "", messageContent)

	draftText, err := pipeline.GenerateDraft(ctx, userID, msg)
	if err != nil {
		s.log.Error("draft from instructions: generation failed", "error", err)
		if isVaultError(err) {
			_ = updateDraftModalError(ctx, botToken, viewID, s.vaultLockedSlackMessage(), meta)
		} else {
			_ = updateDraftModalError(ctx, botToken, viewID, "Draft generation failed. Please try again.", meta)
		}
		return
	}

	if err := updateDraftModalWithDraft(ctx, botToken, viewID, draftText, meta); err != nil {
		s.log.Error("draft from instructions: failed to update modal", "error", err)
	}
}

// handleSlackAgentSend handles the send_draft_agent action from an agent DM.
// It posts the draft text to the target channel as the user.
func (s *apiServer) handleSlackAgentSend(ctx context.Context, userID, targetChannel, targetThreadTS, draftBody string) error {
	return s.sendDraftMessage(ctx, userID, targetChannel, draftBody, targetThreadTS)
}

// buildStreamingPipeline resolves the user's vault and returns a configured pipeline.
// Returns nil if the pipeline is not configured or the vault is locked.
// Deprecated: use resolvePipelineVault instead.
func (s *apiServer) buildStreamingPipeline(userID string) *draft.Pipeline {
	return s.resolvePipelineVault(userID)
}
