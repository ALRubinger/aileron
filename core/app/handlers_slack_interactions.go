package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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
	View *struct {
		ID              string `json:"id"`
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           *struct {
			Values map[string]map[string]struct {
				Value string `json:"value"`
			} `json:"values"`
		} `json:"state"`
	} `json:"view,omitempty"` // view_submission only
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
		go s.handleMessageShortcut(context.Background(), payload)

	case "view_submission":
		go s.processViewSubmission(payload)

	default:
		s.log.Debug("slack interaction: unhandled type", "type", payload.Type)
	}
}

// deprecatedDraftActions are the old ephemeral draft button actions that have
// been replaced by the agent framework.
var deprecatedDraftActions = map[string]bool{
	"approve_draft": true,
	"edit_draft":    true,
	"discard_draft": true,
}

// processInteraction handles block_actions button clicks.
func (s *apiServer) processInteraction(actionID, actionValue string, payload slackInteractionPayload) {
	ctx := context.Background()

	// Old ephemeral draft buttons → graceful deprecation message.
	if deprecatedDraftActions[actionID] {
		s.respondToInteraction(payload.ResponseURL,
			"This draft interface has been replaced. Message Aileron directly or use the message shortcut to draft replies.")
		return
	}

	switch actionID {
	case "send_draft_agent":
		// Send button from agent DM — value is JSON with target info.
		var sendMeta struct {
			Channel  string `json:"channel"`
			ThreadTS string `json:"thread_ts"`
			Body     string `json:"body"`
			UserID   string `json:"user_id"`
		}
		if err := json.Unmarshal([]byte(actionValue), &sendMeta); err != nil {
			s.log.Error("interaction: failed to parse send metadata", "error", err)
			return
		}
		if err := s.sendDraftMessage(ctx, sendMeta.UserID, sendMeta.Channel, sendMeta.Body, sendMeta.ThreadTS); err != nil {
			s.log.Error("interaction: failed to send from agent DM", "error", err)
			return
		}
		s.log.Info("interaction: draft sent from agent DM",
			"user_id", sendMeta.UserID,
			"channel", sendMeta.Channel,
		)

	default:
		s.log.Debug("interaction: unknown action", "action_id", actionID)
	}
}

// processViewSubmission handles a modal submission (user clicked "Send").
func (s *apiServer) processViewSubmission(payload slackInteractionPayload) {
	if payload.View == nil || payload.View.CallbackID != draftModalCallbackID {
		return
	}

	meta, err := parseDraftModalMeta(payload.View.PrivateMetadata)
	if err != nil {
		s.log.Error("view submission: failed to parse metadata", "error", err)
		return
	}

	// Extract draft text from the modal state.
	draftText := ""
	if payload.View.State != nil {
		if block, ok := payload.View.State.Values[draftInputBlockID]; ok {
			if input, ok := block[draftInputActionID]; ok {
				draftText = input.Value
			}
		}
	}

	if draftText == "" {
		s.log.Warn("view submission: empty draft text")
		return
	}

	// Resolve Aileron user from Slack user ID.
	teamID := ""
	if payload.Team != nil {
		teamID = payload.Team.ID
	}
	userID, err := s.resolveAileronUserBySlack(context.Background(), meta.UserID, teamID)
	if err != nil {
		s.log.Error("view submission: failed to resolve user", "error", err)
		return
	}

	// Send the draft as the user.
	if err := s.sendDraftMessage(context.Background(), userID, meta.TargetChannel, draftText, meta.TargetThreadTS); err != nil {
		s.log.Error("view submission: failed to send", "error", err)
	} else {
		s.log.Info("view submission: draft sent",
			"user_id", userID,
			"channel", meta.TargetChannel,
			"draft_length", len(draftText),
		)
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
