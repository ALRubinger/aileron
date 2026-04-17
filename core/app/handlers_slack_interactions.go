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

// slackInteractionPayload is the payload Slack sends when a user clicks
// a button or interacts with a Block Kit element.
type slackInteractionPayload struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"` // draft ID
	} `json:"actions"`
	ResponseURL string `json:"response_url"`
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

	if payload.Type != "block_actions" || len(payload.Actions) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	action := payload.Actions[0]
	draftID := action.Value

	// Return 200 immediately — Slack requires fast response.
	w.WriteHeader(http.StatusOK)

	// Process async.
	go s.processInteraction(action.ActionID, draftID, payload)
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
