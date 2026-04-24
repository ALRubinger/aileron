package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/slack-go/slack"
)

const (
	intentModalCallbackID  = "aileron_intent_modal"
	intentContentBlockID   = "intent_content"
	intentContentActionID  = "intent_content_text"
)

// IntentModalMeta carries intent details across the modal lifecycle.
type IntentModalMeta struct {
	IntentType string `json:"intent_type"`
	UserID     string `json:"user_id"`     // Aileron user ID
	ChannelID  string `json:"channel_id"`
	ActionJSON string `json:"action_json"` // serialized model.ActionIntent
}

// handleSlackCommand handles POST /v1/webhooks/slack/commands.
// This is the endpoint for the /aileron slash command. Every request goes
// through intent resolution — the LLM decides whether it's informational
// or a write action.
func (s *apiServer) handleSlackCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	if !s.verifySlackSignature(r, body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	params, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	text := params.Get("text")
	teamID := params.Get("team_id")
	channelID := params.Get("channel_id")
	triggerID := params.Get("trigger_id")
	slackUserID := params.Get("user_id")
	responseURL := params.Get("response_url")

	// Respond immediately — Slack requires a response within 3s.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response_type": "ephemeral",
		"text":          "> /aileron " + text + "\n\n:hourglass_flowing_sand: Working on it...",
	})

	go s.processSlashCommand(context.Background(), teamID, channelID, slackUserID, text, triggerID, responseURL)
}

// processSlashCommand resolves the user's intent and presents the result.
// Write actions open a modal with an editable preview. Informational
// requests respond via response_url.
func (s *apiServer) processSlashCommand(ctx context.Context, teamID, channelID, slackUserID, text, triggerID, responseURL string) {
	botToken, err := s.resolveWorkspaceBotToken(ctx, teamID)
	if err != nil {
		s.log.Error("slash command: failed to resolve bot token", "error", err)
		s.respondViaURL(responseURL, ":x: Failed to resolve workspace configuration.")
		return
	}

	userID, err := s.resolveAileronUserBySlack(ctx, slackUserID, teamID)
	if err != nil {
		s.log.Error("slash command: failed to resolve user", "error", err)
		s.respondViaURL(responseURL, ":x: Could not find your Aileron account.")
		return
	}

	pipeline := s.resolvePipelineVault(userID)
	if pipeline == nil {
		if s.draftPipeline == nil {
			s.respondViaURL(responseURL, ":x: Draft generation is not configured.")
		} else if s.enclaveClient != nil && s.enclaveReady(ctx) != nil {
			s.respondViaURL(responseURL, ":warning: Aileron is still starting up. Please try again in about 30 seconds.")
		} else {
			s.respondViaURL(responseURL, s.vaultLockedSlackMessage())
		}
		return
	}

	displayName := comms.ResolveSlackDisplayName(ctx, botToken, slackUserID)
	msg := comms.BuildIncomingMessage("", channelID, displayName, text)

	intents, err := pipeline.ResolveIntents(ctx, userID, msg)
	if err != nil {
		s.log.Error("slash command: intent resolution failed", "user_id", userID, "error", err)
		if isVaultError(err) {
			s.respondViaURL(responseURL, s.credentialErrorMessage(err))
		} else {
			s.respondViaURL(responseURL, "> /aileron "+text+"\n\n:x: Something went wrong. Please try again.")
		}
		return
	}

	if len(intents) == 0 {
		s.respondViaURL(responseURL, "> /aileron "+text+"\n\n:shrug: I couldn't determine what to do.")
		return
	}

	intent := intents[0]

	if intent.IsWriteAction() {
		// Write action — open a modal with editable preview.
		if triggerID == "" {
			// No trigger_id (rare) — fall back to response_url.
			s.respondViaURL(responseURL, formatIntentPreview(intent))
			return
		}
		s.openIntentModal(ctx, botToken, triggerID, userID, channelID, intent)
	} else {
		// Informational — respond via response_url.
		s.respondViaURL(responseURL, "> /aileron "+text+"\n\n"+intent.ResponseText)
	}

	s.log.Info("slash command: intent delivered",
		"user_id", userID,
		"type", string(intent.Type),
		"is_write", intent.IsWriteAction(),
	)
}

// openIntentModal opens a Slack modal with an editable preview of a write
// action intent. The user can edit the content and click Send to approve.
func (s *apiServer) openIntentModal(ctx context.Context, botToken, triggerID, userID, channelID string, intent draft.ResolvedIntent) {
	preview := formatIntentPreview(intent)

	// Store intent details in modal metadata so the submission handler
	// can create and execute the intent.
	intentJSON, _ := json.Marshal(intent.Action)
	meta := IntentModalMeta{
		IntentType: string(intent.Type),
		UserID:     userID,
		ChannelID:  channelID,
		ActionJSON: string(intentJSON),
	}
	metaJSON, _ := json.Marshal(meta)

	draftInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "Edit the content...", false, false),
		intentContentActionID,
	)
	draftInput.Multiline = true
	if len(preview) > maxModalTextLength {
		preview = preview[:maxModalTextLength]
	}
	draftInput.InitialValue = preview

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*"+intentTypeLabel(intent.Type)+"*", false, false),
			nil, nil,
		),
		slack.NewInputBlock(
			intentContentBlockID,
			slack.NewTextBlockObject(slack.PlainTextType, "Content", false, false),
			nil,
			draftInput,
		),
	}

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      intentModalCallbackID,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Review & Send", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Send", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		PrivateMetadata: string(metaJSON),
		Blocks: slack.Blocks{
			BlockSet: blocks,
		},
	}

	if _, err := comms.OpenModal(ctx, botToken, triggerID, view); err != nil {
		s.log.Error("slash command: failed to open intent modal", "error", err)
	}
}

// formatIntentPreview generates a readable text preview of a write action intent.
func formatIntentPreview(intent draft.ResolvedIntent) string {
	if intent.Action == nil {
		return intent.Summary
	}
	switch {
	case intent.Action.Domain.Email != nil:
		e := intent.Action.Domain.Email
		preview := "To: " + formatRecipientList(e.To) + "\n"
		if e.Subject != "" {
			preview += "Subject: " + e.Subject + "\n"
		}
		preview += "\n" + e.BodyText
		return preview

	case intent.Action.Domain.Calendar != nil:
		c := intent.Action.Domain.Calendar
		preview := "Title: " + c.Title + "\n"
		if c.StartTime != nil {
			preview += "When: " + c.StartTime.Format("Mon Jan 2, 3:04 PM")
			if c.EndTime != nil {
				preview += " – " + c.EndTime.Format("3:04 PM")
			}
			preview += "\n"
		}
		if c.Location != "" {
			preview += "Location: " + c.Location + "\n"
		}
		if c.Description != "" {
			preview += "\n" + c.Description
		}
		return preview

	case intent.Action.Domain.Git != nil:
		g := intent.Action.Domain.Git
		if g.IssueTitle != "" {
			preview := "Repo: " + g.Repository + "\nTitle: " + g.IssueTitle + "\n"
			if g.IssueBody != "" {
				preview += "\n" + g.IssueBody
			}
			return preview
		}
		return intent.Summary

	default:
		return intent.Summary
	}
}

// intentTypeLabel returns a human-readable label for an intent type.
func intentTypeLabel(t draft.IntentType) string {
	switch t {
	case draft.IntentTypeEmailSend:
		return "Send Email"
	case draft.IntentTypeEmailDraft:
		return "Save Email Draft"
	case draft.IntentTypeCalendarEventCreate:
		return "Create Calendar Event"
	case draft.IntentTypeGitIssueCreate:
		return "Create GitHub Issue"
	case draft.IntentTypeGitIssueComment:
		return "Comment on GitHub Issue"
	case draft.IntentTypeSlackSend:
		return "Send Slack Message"
	default:
		return string(t)
	}
}

// respondViaURL sends an ephemeral response update via Slack's response_url.
func (s *apiServer) respondViaURL(responseURL, text string) {
	if responseURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"response_type":    "ephemeral",
		"replace_original": true,
		"text":             text,
	})
	resp, err := http.Post(responseURL, "application/json", bytes.NewReader(body))
	if err != nil {
		s.log.Error("slash command: failed to respond via URL", "error", err)
		return
	}
	resp.Body.Close()
}
