package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ALRubinger/aileron/core/model"
)

// slackInteractionPayload is the payload Slack sends for interactive events:
// block_actions (button clicks), message_action (message shortcuts), and
// view_submission (modal submits).
type slackInteractionPayload struct {
	Type       string `json:"type"` // "block_actions", "message_action", "view_submission"
	CallbackID string `json:"callback_id,omitempty"` // message_action callback ID
	TriggerID  string `json:"trigger_id,omitempty"`  // for opening modals
	User       struct {
		ID string `json:"id"`
	} `json:"user"`
	Team *struct {
		ID string `json:"id"`
	} `json:"team,omitempty"`
	Channel *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel,omitempty"`
	Message *slackActionMessage `json:"message,omitempty"` // message_action only: the message acted on
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"` // draft ID or JSON metadata
	} `json:"actions"`
	ResponseURL string `json:"response_url"`
}

// slackActionMessage is the message that was acted on in a message_action.
type slackActionMessage struct {
	Text     string `json:"text"`
	User     string `json:"user"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

// handleSlackInteraction handles POST /v1/webhooks/slack/interactions.
// Slack sends interaction payloads as application/x-www-form-urlencoded
// with a "payload" field containing JSON.
func (s *apiServer) handleSlackInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Verify Slack signature (same mechanism as events webhook).
	if !s.verifySlackSignature(r, body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Parse the URL-encoded payload.
	values, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	payloadJSON := values.Get("payload")
	if payloadJSON == "" {
		http.Error(w, "missing payload", http.StatusBadRequest)
		return
	}

	var payload slackInteractionPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		http.Error(w, "invalid payload JSON", http.StatusBadRequest)
		return
	}

	// Return 200 immediately — Slack requires fast response.
	w.WriteHeader(http.StatusOK)

	switch payload.Type {
	case "block_actions":
		if len(payload.Actions) == 0 {
			return
		}
		action := payload.Actions[0]
		go s.processInteraction(action.ActionID, action.Value, payload)

	case "message_action":
		go s.processMessageShortcut(payload)

	default:
		s.log.Debug("slack interaction: unhandled type", "type", payload.Type)
	}
}

// processInteraction handles a button click on a draft ephemeral message.
func (s *apiServer) processInteraction(actionID, draftID string, payload slackInteractionPayload) {
	ctx := context.Background()

	draft, err := s.drafts.Get(ctx, draftID)
	if err != nil {
		s.log.Error("interaction: draft not found", "draft_id", draftID, "error", err)
		return
	}

	if draft.Status != model.DraftStatusPending {
		s.log.Debug("interaction: draft already actioned", "draft_id", draftID, "status", draft.Status)
		return
	}

	// Look up the Aileron user from the Slack user ID.
	// The draft already has the UserID, so we use that.

	switch actionID {
	case "approve_draft":
		if err := s.sendDraftMessage(ctx, draft.UserID, draft.Channel, draft.DraftBody, draft.MessageTS); err != nil {
			s.log.Error("interaction: failed to send approved draft", "draft_id", draftID, "error", err)
			s.respondToInteraction(payload.ResponseURL, "Failed to send: "+err.Error())
			return
		}
		draft.Status = model.DraftStatusApproved
		draft.SentBody = draft.DraftBody
		draft.UpdatedAt = time.Now().UTC()
		s.drafts.Update(ctx, draft)
		s.recordFeedback(ctx, draft, model.FeedbackSignalApproved)
		s.respondToInteraction(payload.ResponseURL, "Sent.")

	case "discard_draft":
		draft.Status = model.DraftStatusDiscarded
		draft.UpdatedAt = time.Now().UTC()
		s.drafts.Update(ctx, draft)
		s.recordFeedback(ctx, draft, model.FeedbackSignalDiscarded)
		s.respondToInteraction(payload.ResponseURL, "Draft discarded.")

	case "edit_draft":
		// For MVP, respond with the draft text for the user to copy/edit manually.
		// A full Slack modal (views.open) for inline editing is a follow-up.
		s.respondToInteraction(payload.ResponseURL,
			fmt.Sprintf("Edit and send manually:\n\n```%s```", draft.DraftBody))

	default:
		s.log.Debug("interaction: unknown action", "action_id", actionID)
	}
}

// processMessageShortcut handles a message_action interaction (message shortcut).
// This is triggered when a user selects "Draft reply with Aileron" from a message's
// ⋯ menu. Currently a stub — full implementation in PR 2.
func (s *apiServer) processMessageShortcut(payload slackInteractionPayload) {
	channelName := ""
	if payload.Channel != nil {
		channelName = payload.Channel.Name
	}
	messagePreview := ""
	if payload.Message != nil {
		messagePreview = payload.Message.Text
		if len(messagePreview) > 80 {
			messagePreview = messagePreview[:80] + "..."
		}
	}
	s.log.Info("slack interaction: message shortcut received",
		"callback_id", payload.CallbackID,
		"user", payload.User.ID,
		"channel", channelName,
		"message_preview", messagePreview,
		"trigger_id", payload.TriggerID,
	)
}

// respondToInteraction sends a response to Slack's response_url,
// replacing the ephemeral message with a status update.
func (s *apiServer) respondToInteraction(responseURL, text string) {
	if responseURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"replace_original": true,
		"text":             text,
	})
	resp, err := http.Post(responseURL, "application/json", bytes.NewReader(body))
	if err != nil {
		s.log.Error("interaction: failed to respond", "error", err)
		return
	}
	resp.Body.Close()
}
