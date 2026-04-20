package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
)

// handleCreateDraft creates a draft directly (for programmatic/admin use).
// POST /v1/drafts
func (s *apiServer) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	var req struct {
		Service     string `json:"service"`
		Channel     string `json:"channel"`
		Author      string `json:"author"`
		MessageBody string `json:"message_body"`
		MessageTS   string `json:"message_ts"`
		DraftBody   string `json:"draft_body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	if req.DraftBody == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "draft_body is required")
		return
	}

	now := time.Now().UTC()
	d := model.Draft{
		ID:          "dft_" + s.newID(),
		UserID:      userID,
		Status:      model.DraftStatusPending,
		Service:     req.Service,
		Channel:     req.Channel,
		Author:      req.Author,
		MessageBody: req.MessageBody,
		MessageTS:   req.MessageTS,
		DraftBody:   req.DraftBody,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.drafts.Create(r.Context(), d); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, d)
}

// handleListDrafts returns pending drafts for the authenticated user.
// GET /v1/drafts?status=pending
func (s *apiServer) handleListDrafts(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	filter := store.DraftFilter{UserID: userID}
	if statusParam := r.URL.Query().Get("status"); statusParam != "" {
		status := model.DraftStatus(statusParam)
		filter.Status = &status
	}

	drafts, err := s.drafts.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if drafts == nil {
		drafts = []model.Draft{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": drafts})
}

// handleGetDraft returns a specific draft with ownership check.
// GET /v1/drafts/{draft_id}
func (s *apiServer) handleGetDraft(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	draftID := extractDraftID(r.URL.Path)
	if draftID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "draft_id is required")
		return
	}

	draft, err := s.drafts.Get(r.Context(), draftID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if draft.UserID != userID {
		writeError(w, http.StatusNotFound, "not_found", "draft not found")
		return
	}

	writeJSON(w, http.StatusOK, draft)
}

// handleApproveDraft approves a draft and sends it as-is.
// POST /v1/drafts/{draft_id}/approve
func (s *apiServer) handleApproveDraft(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	draftID := extractDraftIDFromAction(r.URL.Path, "/approve")
	draft, err := s.getDraftForAction(w, r, draftID, userID)
	if err != nil {
		return // error already written
	}

	if err := s.sendDraftMessage(r.Context(), draft.UserID, draft.Channel, draft.DraftBody, draft.MessageTS); err != nil {
		s.log.Error("failed to send draft", "draft_id", draftID, "error", err)
		writeError(w, http.StatusInternalServerError, "send_error", "failed to send message: "+err.Error())
		return
	}

	draft.Status = model.DraftStatusApproved
	draft.SentBody = draft.DraftBody
	draft.UpdatedAt = time.Now().UTC()
	s.drafts.Update(r.Context(), draft)

	s.recordFeedback(r.Context(), draft, model.FeedbackSignalApproved)
	writeJSON(w, http.StatusOK, draft)
}

// handleEditDraft edits a draft body and sends the revised version.
// POST /v1/drafts/{draft_id}/edit  { "body": "revised text" }
func (s *apiServer) handleEditDraft(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	draftID := extractDraftIDFromAction(r.URL.Path, "/edit")
	draft, err := s.getDraftForAction(w, r, draftID, userID)
	if err != nil {
		return
	}

	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "body field is required")
		return
	}

	if err := s.sendDraftMessage(r.Context(), draft.UserID, draft.Channel, req.Body, draft.MessageTS); err != nil {
		s.log.Error("failed to send edited draft", "draft_id", draftID, "error", err)
		writeError(w, http.StatusInternalServerError, "send_error", "failed to send message: "+err.Error())
		return
	}

	draft.Status = model.DraftStatusEdited
	draft.SentBody = req.Body
	draft.UpdatedAt = time.Now().UTC()
	s.drafts.Update(r.Context(), draft)

	s.recordFeedback(r.Context(), draft, model.FeedbackSignalEdited)
	writeJSON(w, http.StatusOK, draft)
}

// handleDiscardDraft discards a draft without sending.
// POST /v1/drafts/{draft_id}/discard
func (s *apiServer) handleDiscardDraft(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	draftID := extractDraftIDFromAction(r.URL.Path, "/discard")
	draft, err := s.getDraftForAction(w, r, draftID, userID)
	if err != nil {
		return
	}

	draft.Status = model.DraftStatusDiscarded
	draft.UpdatedAt = time.Now().UTC()
	s.drafts.Update(r.Context(), draft)

	s.recordFeedback(r.Context(), draft, model.FeedbackSignalDiscarded)
	writeJSON(w, http.StatusOK, draft)
}

// getDraftForAction loads a draft, checks ownership and status.
// Writes error response and returns error if the draft can't be acted on.
func (s *apiServer) getDraftForAction(w http.ResponseWriter, r *http.Request, draftID, userID string) (model.Draft, error) {
	if draftID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "draft_id is required")
		return model.Draft{}, errBadRequest
	}

	draft, err := s.drafts.Get(r.Context(), draftID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "draft not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return model.Draft{}, err
	}

	if draft.UserID != userID {
		writeError(w, http.StatusNotFound, "not_found", "draft not found")
		return model.Draft{}, errNotFound
	}

	if draft.Status != model.DraftStatusPending {
		writeError(w, http.StatusConflict, "already_actioned", "draft has already been "+string(draft.Status))
		return model.Draft{}, errConflict
	}

	return draft, nil
}

// EphemeralPoster is the function used to post ephemeral draft messages.
// Defaults to comms.PostEphemeralDraft. Override in tests.
type EphemeralPoster func(ctx context.Context, msg comms.SlackDraftMessage) error

// ThreadChecker checks if a Slack thread has replies.
// Defaults to comms.SlackThreadHasReplies. Override in tests.
type ThreadChecker func(ctx context.Context, botToken, channel, messageTS string) bool

// SlackSender is the function used to send messages to Slack.
// Defaults to comms.SendSlackMessage. Override in tests.
type SlackSender func(ctx context.Context, token, channel, body, threadTS string) error

// sendDraftMessage sends a message to Slack using the user's connected account token.
// When threadTS is non-empty the message is posted as a threaded reply.
func (s *apiServer) sendDraftMessage(ctx context.Context, userID, channel, body, threadTS string) error {
	slackProvider := model.ConnectedAccountProviderSlack
	accounts, err := s.connectedAccounts.List(ctx, store.ConnectedAccountFilter{
		UserID:   userID,
		Provider: &slackProvider,
	})
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return errNoSlackAccount
	}

	vlt := s.userVault(userID)
	secret, err := vlt.Get(ctx, accounts[0].VaultPath())
	if err != nil {
		return err
	}

	var tokenData map[string]string
	if err := json.Unmarshal(secret.Value, &tokenData); err != nil {
		return err
	}

	token := tokenData["access_token"]
	if token == "" {
		return errNoSlackToken
	}

	sender := s.slackSender
	if sender == nil {
		sender = comms.SendSlackMessage
	}
	return sender(ctx, token, channel, body, threadTS)
}

// extractDraftID extracts the draft ID from /v1/drafts/{draft_id}
func extractDraftID(path string) string {
	const prefix = "/v1/drafts/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	id := strings.TrimPrefix(path, prefix)
	// Remove trailing slash if present.
	id = strings.TrimSuffix(id, "/")
	return id
}

// extractDraftIDFromAction extracts the draft ID from /v1/drafts/{draft_id}/action
func extractDraftIDFromAction(path, action string) string {
	const prefix = "/v1/drafts/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimSuffix(rest, action)
	rest = strings.TrimSuffix(rest, "/")
	return rest
}

// handleDraftAction routes POST /v1/drafts/{id}/action to the correct handler.
func (s *apiServer) handleDraftAction(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/approve"):
		s.handleApproveDraft(w, r)
	case strings.HasSuffix(path, "/edit"):
		s.handleEditDraft(w, r)
	case strings.HasSuffix(path, "/discard"):
		s.handleDiscardDraft(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown draft action")
	}
}

// recordFeedback writes a feedback signal for a draft action.
func (s *apiServer) recordFeedback(ctx context.Context, draft model.Draft, signal model.FeedbackSignal) {
	if s.feedback == nil {
		return
	}
	fb := model.DraftFeedback{
		ID:        "fb_" + s.newID(),
		UserID:    draft.UserID,
		DraftID:   draft.ID,
		Signal:    signal,
		Service:   draft.Service,
		Channel:   draft.Channel,
		DraftBody: draft.DraftBody,
		SentBody:  draft.SentBody,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.feedback.Create(ctx, fb); err != nil {
		s.log.Error("failed to record feedback", "draft_id", draft.ID, "signal", signal, "error", err)
	}
}

// handleCreateFeedback creates a feedback signal directly (for programmatic/admin use).
// POST /v1/feedback
func (s *apiServer) handleCreateFeedback(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	if s.feedback == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "feedback store not configured")
		return
	}

	var req struct {
		DraftID   string   `json:"draft_id"`
		Signal    string   `json:"signal"`
		Service   string   `json:"service"`
		Channel   string   `json:"channel"`
		DraftBody string   `json:"draft_body"`
		SentBody  string   `json:"sent_body"`
		ToolsUsed []string `json:"tools_used"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	if req.Signal == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "signal is required")
		return
	}

	fb := model.DraftFeedback{
		ID:        "fb_" + s.newID(),
		UserID:    userID,
		DraftID:   req.DraftID,
		Signal:    model.FeedbackSignal(req.Signal),
		Service:   req.Service,
		Channel:   req.Channel,
		DraftBody: req.DraftBody,
		SentBody:  req.SentBody,
		ToolsUsed: req.ToolsUsed,
		CreatedAt: time.Now().UTC(),
	}

	if err := s.feedback.Create(r.Context(), fb); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, fb)
}

// handleListFeedback returns the user's draft feedback signals.
// GET /v1/feedback?signal=edited
func (s *apiServer) handleListFeedback(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	if s.feedback == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []model.DraftFeedback{}})
		return
	}

	filter := store.DraftFeedbackFilter{UserID: userID}
	if signalParam := r.URL.Query().Get("signal"); signalParam != "" {
		signal := model.FeedbackSignal(signalParam)
		filter.Signal = &signal
	}

	feedback, err := s.feedback.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if feedback == nil {
		feedback = []model.DraftFeedback{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": feedback})
}

// slackCredentials holds resolved Slack bot token and user ID for a user.
type slackCredentials struct {
	BotToken    string
	SlackUserID string
}

// resolveSlackCredentials looks up the user's connected Slack account and
// retrieves their bot token and Slack user ID from the vault.
// If the user has an active KEK session, the vault is wrapped with
// UserScopedVault for transparent decryption.
func (s *apiServer) resolveSlackCredentials(ctx context.Context, userID string) (*slackCredentials, error) {
	slackProvider := model.ConnectedAccountProviderSlack
	accounts, err := s.connectedAccounts.List(ctx, store.ConnectedAccountFilter{
		UserID:   userID,
		Provider: &slackProvider,
	})
	if err != nil || len(accounts) == 0 {
		return nil, fmt.Errorf("no slack account for user %s", userID)
	}

	acct := accounts[0]

	vlt := s.userVault(userID)
	secret, err := vlt.Get(ctx, acct.VaultPath())
	if err != nil {
		return nil, fmt.Errorf("failed to get vault token: %w", err)
	}

	var tokenData map[string]string
	if err := json.Unmarshal(secret.Value, &tokenData); err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	botToken := tokenData["bot_access_token"]
	if botToken == "" {
		return nil, fmt.Errorf("no bot token — Slack app needs chat:write bot scope")
	}

	slackUserID := acct.ExternalUserID
	if slackUserID == "" {
		return nil, fmt.Errorf("no slack user ID on connected account")
	}

	return &slackCredentials{BotToken: botToken, SlackUserID: slackUserID}, nil
}

// EphemeralTextPoster posts a simple text ephemeral message.
// Defaults to comms.PostEphemeralText. Override in tests.
type EphemeralTextPoster func(ctx context.Context, botToken, channel, userID, text, threadTS string) error

// postDraftingIndicator posts a transient "Drafting a reply..." ephemeral
// message so the user gets immediate feedback while the LLM works.
func (s *apiServer) postDraftingIndicator(ctx context.Context, creds *slackCredentials, channel, messageTS string) {
	// Resolve thread safety: only post in-thread if thread has replies.
	checker := s.threadChecker
	if checker == nil {
		checker = comms.SlackThreadHasReplies
	}
	threadTS := ""
	if messageTS != "" {
		if checker(ctx, creds.BotToken, channel, messageTS) {
			threadTS = messageTS
		}
	}

	poster := s.ephemeralTextPoster
	if poster == nil {
		poster = comms.PostEphemeralText
	}
	if err := poster(ctx, creds.BotToken, channel, creds.SlackUserID, ":writing_hand: Drafting a reply…", threadTS); err != nil {
		s.log.Error("drafting indicator: failed to post", "error", err)
	}
}

// deliverEphemeralDraft posts an ephemeral Block Kit message in the Slack
// channel where the original message was, showing the draft with
// approve/edit/discard buttons. Only the target user sees it.
func (s *apiServer) deliverEphemeralDraft(ctx context.Context, userID string, draft model.Draft) {
	creds, err := s.resolveSlackCredentials(ctx, userID)
	if err != nil {
		s.log.Error("ephemeral: "+err.Error(), "user_id", userID)
		return
	}

	// Only post in-thread if the thread already has replies. Slack silently
	// drops ephemeral messages in threads with no replies. If no thread
	// exists yet, post at channel level so the user sees the draft.
	checker := s.threadChecker
	if checker == nil {
		checker = comms.SlackThreadHasReplies
	}
	threadTS := ""
	if draft.MessageTS != "" {
		if checker(ctx, creds.BotToken, draft.Channel, draft.MessageTS) {
			threadTS = draft.MessageTS
		}
	}

	poster := s.ephemeralPoster
	if poster == nil {
		poster = comms.PostEphemeralDraft
	}
	err = poster(ctx, comms.SlackDraftMessage{
		BotToken: creds.BotToken,
		Channel:  draft.Channel,
		UserID:   creds.SlackUserID,
		DraftID:  draft.ID,
		Author:   draft.Author,
		Body:     draft.MessageBody,
		Draft:    draft.DraftBody,
		ThreadTS: threadTS,
	})
	if err != nil {
		s.log.Error("ephemeral: failed to post", "user_id", userID, "draft_id", draft.ID, "error", err)
		return
	}

	s.log.Info("ephemeral draft delivered",
		"draft_id", draft.ID,
		"user_id", userID,
		"channel", draft.Channel)
}

// handleIncomingSlackMessage processes an incoming Slack message that mentions
// the user. It posts a drafting indicator, generates a draft via the LLM
// pipeline, stores it, and delivers an ephemeral preview with action buttons.
func (s *apiServer) handleIncomingSlackMessage(ctx context.Context, userID string, msg comms.IncomingMessage) {
	s.log.Info("slack cloud message received",
		"user_id", userID,
		"channel", msg.Channel,
		"author", msg.Author,
		"preview", msg.Body[:min(len(msg.Body), 80)])

	if s.draftPipeline == nil {
		return
	}

	// Resolve user's KEK for vault decryption. If locked, skip draft generation.
	pipeline := s.draftPipeline
	if kek := s.getUserKEK(userID); kek != nil {
		pipeline = pipeline.WithVault(vault.NewUserScopedVault(s.vault, kek))
		defer zeroBytes(kek)
	} else if s.kekSessionCache != nil {
		s.log.Warn("vault locked for user during draft generation — skipping",
			"user_id", userID)
		return
	}

	// Post immediate "drafting..." indicator so the user
	// knows Aileron is working while the LLM runs.
	creds, err := s.resolveSlackCredentials(ctx, userID)
	if err != nil {
		s.log.Error("failed to resolve slack credentials", "user_id", userID, "error", err)
	} else {
		s.postDraftingIndicator(ctx, creds, msg.Channel, msg.ID)
	}

	draftText, err := pipeline.GenerateDraft(ctx, userID, msg)
	if err != nil {
		s.log.Error("draft generation failed", "user_id", userID, "error", err)
		return
	}

	d, err := s.createDraftFromMessage(ctx, userID, msg, draftText)
	if err != nil {
		s.log.Error("failed to store draft", "error", err)
		return
	}
	s.log.Info("draft pending review",
		"draft_id", d.ID,
		"user_id", userID,
		"channel", msg.Channel,
		"draft_length", len(draftText))

	// Post ephemeral draft preview in Slack.
	s.deliverEphemeralDraft(ctx, userID, d)
}

// createDraftFromMessage stores a generated draft as pending in the draft store.
// Extracted from the onSlackMessage closure so it can be unit tested.
func (s *apiServer) createDraftFromMessage(ctx context.Context, userID string, msg comms.IncomingMessage, draftText string) (model.Draft, error) {
	now := time.Now().UTC()
	d := model.Draft{
		ID:          "dft_" + s.newID(),
		UserID:      userID,
		Status:      model.DraftStatusPending,
		Service:     msg.Service,
		Channel:     msg.Channel,
		Author:      msg.Author,
		MessageBody: msg.Body,
		MessageTS:   msg.ID,
		DraftBody:   draftText,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.drafts.Create(ctx, d); err != nil {
		return model.Draft{}, err
	}
	return d, nil
}

// Sentinel errors for draft handlers.
var (
	errBadRequest      = &store.ErrNotFound{Entity: "request", ID: "bad"}
	errNotFound        = &store.ErrNotFound{Entity: "draft", ID: "not_found"}
	errConflict        = &store.ErrNotFound{Entity: "draft", ID: "conflict"}
	errNoSlackAccount  = &store.ErrNotFound{Entity: "slack_account", ID: "missing"}
	errNoSlackToken    = &store.ErrNotFound{Entity: "slack_token", ID: "missing"}
)
