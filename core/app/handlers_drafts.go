package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

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

	if err := s.sendDraftMessage(r.Context(), draft.UserID, draft.Channel, draft.DraftBody); err != nil {
		s.log.Error("failed to send draft", "draft_id", draftID, "error", err)
		writeError(w, http.StatusInternalServerError, "send_error", "failed to send message: "+err.Error())
		return
	}

	draft.Status = model.DraftStatusApproved
	draft.SentBody = draft.DraftBody
	draft.UpdatedAt = time.Now().UTC()
	s.drafts.Update(r.Context(), draft)

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

	if err := s.sendDraftMessage(r.Context(), draft.UserID, draft.Channel, req.Body); err != nil {
		s.log.Error("failed to send edited draft", "draft_id", draftID, "error", err)
		writeError(w, http.StatusInternalServerError, "send_error", "failed to send message: "+err.Error())
		return
	}

	draft.Status = model.DraftStatusEdited
	draft.SentBody = req.Body
	draft.UpdatedAt = time.Now().UTC()
	s.drafts.Update(r.Context(), draft)

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

// SlackSender is the function used to send messages to Slack.
// Defaults to comms.SendSlackMessage. Override in tests.
type SlackSender func(ctx context.Context, token, channel, body string) error

// sendDraftMessage sends a message to Slack using the user's connected account token.
func (s *apiServer) sendDraftMessage(ctx context.Context, userID, channel, body string) error {
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

	secret, err := s.vault.Get(ctx, accounts[0].VaultPath())
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
	return sender(ctx, token, channel, body)
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

// Sentinel errors for draft handlers.
var (
	errBadRequest      = &store.ErrNotFound{Entity: "request", ID: "bad"}
	errNotFound        = &store.ErrNotFound{Entity: "draft", ID: "not_found"}
	errConflict        = &store.ErrNotFound{Entity: "draft", ID: "conflict"}
	errNoSlackAccount  = &store.ErrNotFound{Entity: "slack_account", ID: "missing"}
	errNoSlackToken    = &store.ErrNotFound{Entity: "slack_token", ID: "missing"}
)
