package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/policy"
)

// slackInteractionPayload is the payload Slack sends for interactive events:
// block_actions (button clicks), message_action (message shortcuts), and
// view_submission (modal submits).
type slackInteractionPayload struct {
	Type       string `json:"type"`                  // "block_actions", "message_action", "view_submission"
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
	} `json:"view,omitempty"` // view_submission and modal block_actions
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

	// Handle instructions modal submission specially — we need to return a
	// response_action to update the view in-place before generating the draft.
	if payload.Type == "view_submission" && payload.View != nil && payload.View.CallbackID == instructionsModalCallbackID {
		meta, err := parseDraftModalMeta(payload.View.PrivateMetadata)
		if err != nil {
			s.log.Error("instructions submit: failed to parse metadata", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Extract the user's instructions from the modal state.
		if payload.View.State != nil {
			if block, ok := payload.View.State.Values[instructionsInputBlockID]; ok {
				if input, ok := block[instructionsInputActionID]; ok {
					meta.Instructions = input.Value
				}
			}
		}

		// Respond with an updated loading view — keeps the modal open.
		loadingView := buildDraftLoadingView(meta)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"response_action": "update",
			"view":            loadingView,
		})

		// Generate the draft asynchronously.
		go s.generateDraftFromInstructions(context.Background(), payload.View.ID, meta)
		return
	}

	// Handle intent modal submission — user clicked "Send" on a write action preview.
	if payload.Type == "view_submission" && payload.View != nil && payload.View.CallbackID == intentModalCallbackID {
		var intentMeta IntentModalMeta
		if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &intentMeta); err != nil {
			s.log.Error("intent modal submit: failed to parse metadata", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Deserialize the action.
		var action model.ActionIntent
		if err := json.Unmarshal([]byte(intentMeta.ActionJSON), &action); err != nil {
			s.log.Error("intent modal submit: failed to parse action", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Close the modal.
		w.WriteHeader(http.StatusOK)

		// Submit the intent asynchronously.
		go s.executeIntentFromModal(context.Background(), intentMeta, action)
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

// processInteraction handles block_actions button clicks.
func (s *apiServer) processInteraction(actionID, actionValue string, payload slackInteractionPayload) {
	ctx := context.Background()

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

	case "approve_intent":
		// Approve button from intent resolution — submit structured intent
		// for policy evaluation and execution.
		s.processApproveIntent(ctx, actionValue, payload)

	case "cancel_draft":
		// Cancel button — acknowledge with a response URL update if available.
		if payload.ResponseURL != "" {
			s.respondToInteraction(payload.ResponseURL, ":x: Cancelled.")
		}
		s.log.Info("interaction: draft cancelled", "user_id", payload.User.ID)

	case refineActionID:
		s.processRefineDraft(ctx, payload)

	default:
		s.log.Debug("interaction: unknown action", "action_id", actionID)
	}
}

// processRefineDraft handles the "Refine" button click in the draft modal.
// It extracts the current draft and feedback from the view state, runs the
// pipeline's RefineDraft method, and updates the modal with the revised draft.
func (s *apiServer) processRefineDraft(ctx context.Context, payload slackInteractionPayload) {
	if payload.View == nil {
		s.log.Error("refine: no view in payload")
		return
	}

	meta, err := parseDraftModalMeta(payload.View.PrivateMetadata)
	if err != nil {
		s.log.Error("refine: failed to parse metadata", "error", err)
		return
	}

	// Extract current draft and feedback from view state.
	var draftText, feedback string
	if payload.View.State != nil {
		if block, ok := payload.View.State.Values[draftInputBlockID]; ok {
			if input, ok := block[draftInputActionID]; ok {
				draftText = input.Value
			}
		}
		if block, ok := payload.View.State.Values[instructionBlockID]; ok {
			if input, ok := block[instructionActionID]; ok {
				feedback = input.Value
			}
		}
	}

	if draftText == "" {
		s.log.Warn("refine: empty draft text")
		return
	}
	if feedback == "" {
		s.log.Warn("refine: no feedback provided")
		return
	}

	teamID := ""
	if payload.Team != nil {
		teamID = payload.Team.ID
	}

	botToken, err := s.resolveWorkspaceBotToken(ctx, teamID)
	if err != nil {
		s.log.Error("refine: failed to resolve bot token", "error", err)
		return
	}

	// Show loading state while refining.
	_ = updateDraftModalLoading(ctx, botToken, payload.View.ID, "Refining...", meta)

	userID, err := s.resolveAileronUserBySlack(ctx, meta.UserID, teamID)
	if err != nil {
		s.log.Error("refine: failed to resolve user", "error", err)
		_ = updateDraftModalError(ctx, botToken, payload.View.ID, "Could not find your Aileron account.", meta)
		return
	}

	pipeline := s.resolvePipelineVault(userID)
	if pipeline == nil {
		if s.draftPipeline == nil {
			_ = updateDraftModalError(ctx, botToken, payload.View.ID, "Draft generation is not configured.", meta)
		} else {
			_ = updateDraftModalError(ctx, botToken, payload.View.ID, s.vaultLockedSlackMessage(), meta)
		}
		return
	}

	refined, err := pipeline.RefineDraft(ctx, userID, meta.OriginalMessage, draftText, feedback)
	if err != nil {
		s.log.Error("refine: pipeline error", "error", err)
		_ = updateDraftModalError(ctx, botToken, payload.View.ID, "Refinement failed. Please try again.", meta)
		return
	}

	if err := updateDraftModalWithDraft(ctx, botToken, payload.View.ID, refined, meta); err != nil {
		s.log.Error("refine: failed to update modal", "error", err)
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

// processApproveIntent handles the approve_intent button click from the
// Slack agent DM. It submits the structured intent to the policy engine,
// auto-approves if policy requires approval (since the human already clicked
// Send), issues an execution grant, and runs the execution.
func (s *apiServer) processApproveIntent(ctx context.Context, actionValue string, payload slackInteractionPayload) {
	var intentMeta struct {
		Type   string              `json:"type"`
		UserID string              `json:"user_id"`
		Action *model.ActionIntent `json:"action"`
	}
	if err := json.Unmarshal([]byte(actionValue), &intentMeta); err != nil {
		s.log.Error("approve_intent: failed to parse metadata", "error", err)
		if payload.ResponseURL != "" {
			s.respondToInteraction(payload.ResponseURL, ":x: Failed to process approval.")
		}
		return
	}

	if intentMeta.Action == nil {
		s.log.Error("approve_intent: no action in metadata")
		if payload.ResponseURL != "" {
			s.respondToInteraction(payload.ResponseURL, ":x: No action to execute.")
		}
		return
	}

	// Acknowledge immediately.
	if payload.ResponseURL != "" {
		s.respondToInteraction(payload.ResponseURL,
			":hourglass_flowing_sand: Submitting for execution...")
	}

	// Create the intent, evaluate policy, and execute.
	now := time.Now().UTC()
	intentID := "int_" + s.newID()

	// Convert model action to API action for the intent envelope.
	apiAction := api.ActionIntent{
		Type:    intentMeta.Action.Type,
		Summary: intentMeta.Action.Summary,
	}
	// Carry domain through metadata for now — the execution handler
	// uses buildConnectorParams which reads from api.ActionIntent.Domain.
	// Full model→API domain conversion would be ideal but is mechanical.
	apiAction.Metadata = &map[string]interface{}{
		"_model_domain": intentMeta.Action.Domain,
	}

	envelope := api.IntentEnvelope{
		IntentId:    intentID,
		WorkspaceId: "default",
		Agent: api.ActorRef{
			Id:   "aileron_slack",
			Type: api.Agent,
		},
		Action:    apiAction,
		Status:    api.PendingPolicy,
		CreatedAt: now,
		UpdatedAt: now,
		Decision: api.Decision{
			Disposition: api.DecisionDispositionAllow,
			RiskLevel:   api.Low,
		},
	}

	if err := s.intents.Create(ctx, envelope); err != nil {
		s.log.Error("approve_intent: failed to create intent", "error", err)
		return
	}

	// Evaluate policy.
	decision, err := s.policyEngine.Evaluate(ctx, policy.EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "aileron_slack",
		Action:      *intentMeta.Action,
	})
	if err != nil {
		s.log.Error("approve_intent: policy evaluation failed", "error", err)
		return
	}

	s.log.Info("approve_intent: policy evaluated",
		"intent_id", intentID,
		"disposition", string(decision.Disposition),
		"risk_level", string(decision.RiskLevel),
	)

	// For Slack-initiated intents, the human already approved by clicking Send.
	// If policy says Allow or RequireApproval, we proceed. Only Deny blocks.
	if decision.Disposition == model.DispositionDeny {
		envelope.Status = api.Denied
		s.intents.Update(ctx, envelope)
		if payload.ResponseURL != "" {
			reason := decision.DenialReason
			if reason == "" {
				reason = "blocked by policy"
			}
			s.respondToInteraction(payload.ResponseURL,
				":no_entry: Denied by policy: "+reason)
		}
		return
	}

	// Issue execution grant.
	grantID := "grt_" + s.newID()
	grant, err := s.newExecutionGrant(ctx, envelope, grantID, now.Add(5*time.Minute), nil, intentMeta.UserID)
	if err != nil {
		s.log.Error("approve_intent: failed to issue grant capability", "error", err)
		return
	}
	if err := s.grants.Create(ctx, grant); err != nil {
		s.log.Error("approve_intent: failed to create grant", "error", err)
		return
	}

	envelope.Status = api.Approved
	envelope.UpdatedAt = time.Now().UTC()
	s.intents.Update(ctx, envelope)

	s.log.Info("approve_intent: grant issued, executing",
		"intent_id", intentID,
		"grant_id", grantID,
		"action_type", intentMeta.Action.Type,
		"user_id", intentMeta.UserID,
	)

	// Execute the grant — this resolves credentials, calls the connector,
	// and records the execution result.
	result, execErr := s.executeGrant(ctx, grantID, intentMeta.UserID)
	if execErr != nil {
		s.log.Error("approve_intent: execution failed", "error", execErr)
		if payload.ResponseURL != "" {
			s.respondToInteraction(payload.ResponseURL,
				":x: Execution failed: "+execErr.Error())
		}
		return
	}

	if result.Status == api.ExecutionStatusFailed {
		s.log.Error("approve_intent: connector execution failed",
			"execution_id", result.ExecutionID,
			"error", result.Error,
		)
		if payload.ResponseURL != "" {
			errMsg := result.Error
			if errMsg == "" {
				errMsg = "unknown error"
			}
			s.respondToInteraction(payload.ResponseURL,
				":x: Execution failed: "+errMsg)
		}
		return
	}

	s.log.Info("approve_intent: execution succeeded",
		"execution_id", result.ExecutionID,
		"receipt_ref", result.ReceiptRef,
	)
	if payload.ResponseURL != "" {
		msg := ":white_check_mark: Done!"
		if result.ReceiptRef != "" {
			msg += " " + result.ReceiptRef
		}
		s.respondToInteraction(payload.ResponseURL, msg)
	}
}

// executeIntentFromModal handles the submission of an intent modal. It creates
// the intent, evaluates policy, issues a grant, and executes — same flow as
// processApproveIntent but initiated from a modal submission.
func (s *apiServer) executeIntentFromModal(ctx context.Context, meta IntentModalMeta, action model.ActionIntent) {
	now := time.Now().UTC()
	intentID := "int_" + s.newID()

	apiAction := api.ActionIntent{
		Type:    action.Type,
		Summary: action.Summary,
	}
	apiAction.Metadata = &map[string]interface{}{
		"_model_domain": action.Domain,
	}

	envelope := api.IntentEnvelope{
		IntentId:    intentID,
		WorkspaceId: "default",
		Agent:       api.ActorRef{Id: "aileron_slack", Type: api.Agent},
		Action:      apiAction,
		Status:      api.PendingPolicy,
		CreatedAt:   now,
		UpdatedAt:   now,
		Decision:    api.Decision{Disposition: api.DecisionDispositionAllow, RiskLevel: api.Low},
	}

	if err := s.intents.Create(ctx, envelope); err != nil {
		s.log.Error("intent modal: failed to create intent", "error", err)
		return
	}

	decision, err := s.policyEngine.Evaluate(ctx, policy.EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "aileron_slack",
		Action:      action,
	})
	if err != nil {
		s.log.Error("intent modal: policy evaluation failed", "error", err)
		return
	}

	if decision.Disposition == model.DispositionDeny {
		envelope.Status = api.Denied
		s.intents.Update(ctx, envelope)
		s.log.Warn("intent modal: denied by policy", "intent_id", intentID, "reason", decision.DenialReason)
		// TODO: Notify user via Slack DM that the action was denied.
		return
	}

	grantID := "grt_" + s.newID()
	grant, err := s.newExecutionGrant(ctx, envelope, grantID, now.Add(5*time.Minute), nil, meta.UserID)
	if err != nil {
		s.log.Error("intent modal: failed to issue grant capability", "error", err)
		return
	}
	if err := s.grants.Create(ctx, grant); err != nil {
		s.log.Error("intent modal: failed to create grant", "error", err)
		return
	}
	envelope.Status = api.Approved
	envelope.UpdatedAt = time.Now().UTC()
	s.intents.Update(ctx, envelope)

	result, execErr := s.executeGrant(ctx, grantID, meta.UserID)
	if execErr != nil {
		s.log.Error("intent modal: execution failed", "error", execErr)
		return
	}

	if result.Status == api.ExecutionStatusFailed {
		s.log.Error("intent modal: connector failed",
			"execution_id", result.ExecutionID,
			"error", result.Error,
		)
	} else {
		s.log.Info("intent modal: execution succeeded",
			"execution_id", result.ExecutionID,
			"receipt_ref", result.ReceiptRef,
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
