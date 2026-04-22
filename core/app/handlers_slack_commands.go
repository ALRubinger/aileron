package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/vault"
)

// draftKeywords are words that indicate the user wants a draft (not a question).
var draftKeywords = []string{"draft", "write", "compose", "reply", "respond", "message"}

// handleSlackCommand handles POST /v1/webhooks/slack/commands.
// This is the endpoint for the /aileron slash command.
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

	isDraft := isDraftRequest(text)

	if isDraft && triggerID != "" {
		// Draft request — open a modal immediately.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		meta := DraftModalMeta{
			TargetChannel: channelID,
			UserID:        slackUserID,
		}

		go s.processSlashCommandDraft(context.Background(), teamID, slackUserID, text, triggerID, meta)
		return
	}

	// Question or no trigger_id — respond ephemerally.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response_type": "ephemeral",
		"text":          ":hourglass_flowing_sand: Working on it...",
	})

	go s.processSlashCommandQuestion(context.Background(), teamID, slackUserID, text, responseURL)
}

// processSlashCommandDraft handles a draft-type slash command by opening a modal
// and generating a draft asynchronously.
func (s *apiServer) processSlashCommandDraft(ctx context.Context, teamID, slackUserID, text, triggerID string, meta DraftModalMeta) {
	botToken, err := s.resolveWorkspaceBotToken(ctx, teamID)
	if err != nil {
		s.log.Error("slash command draft: failed to resolve bot token", "error", err)
		return
	}

	viewResp, err := openDraftModal(ctx, botToken, triggerID, meta)
	if err != nil {
		s.log.Error("slash command draft: failed to open modal", "error", err)
		return
	}

	userID, err := s.resolveAileronUserBySlack(ctx, slackUserID, teamID)
	if err != nil {
		s.log.Error("slash command draft: failed to resolve user", "error", err)
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Could not find your Aileron account.", meta)
		return
	}

	if s.draftPipeline == nil {
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Draft generation is not configured.", meta)
		return
	}

	pipeline := s.draftPipeline
	if kek := s.getUserKEK(userID); kek != nil {
		pipeline = pipeline.WithVault(vault.NewUserScopedVault(s.vault, kek))
		defer zeroBytes(kek)
	} else if s.kekSessionCache != nil {
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Your vault is locked. Please unlock it first.", meta)
		return
	}

	displayName := comms.ResolveSlackDisplayName(ctx, botToken, slackUserID)
	msg := comms.BuildIncomingMessage("", meta.TargetChannel, displayName, text)
	draftText, err := pipeline.GenerateDraft(ctx, userID, msg)
	if err != nil {
		s.log.Error("slash command draft: generation failed", "error", err)
		_ = updateDraftModalError(ctx, botToken, viewResp.ID, "Draft generation failed. Please try again.", meta)
		return
	}

	if err := updateDraftModalWithDraft(ctx, botToken, viewResp.ID, draftText, meta); err != nil {
		s.log.Error("slash command draft: failed to update modal", "error", err)
	}
}

// processSlashCommandQuestion handles a question-type slash command by running
// the pipeline and updating via response_url.
func (s *apiServer) processSlashCommandQuestion(ctx context.Context, teamID, slackUserID, text, responseURL string) {
	userID, err := s.resolveAileronUserBySlack(ctx, slackUserID, teamID)
	if err != nil {
		s.log.Error("slash command question: failed to resolve user", "error", err)
		s.respondViaURL(responseURL, "Could not find your Aileron account.")
		return
	}

	if s.draftPipeline == nil {
		s.respondViaURL(responseURL, "Draft generation is not configured.")
		return
	}

	pipeline := s.draftPipeline
	if kek := s.getUserKEK(userID); kek != nil {
		pipeline = pipeline.WithVault(vault.NewUserScopedVault(s.vault, kek))
		defer zeroBytes(kek)
	} else if s.kekSessionCache != nil {
		s.respondViaURL(responseURL, "Your vault is locked. Please unlock it first.")
		return
	}

	botToken, _ := s.resolveWorkspaceBotToken(ctx, teamID)
	displayName := comms.ResolveSlackDisplayName(ctx, botToken, slackUserID)
	msg := comms.BuildIncomingMessage("", "", displayName, text)
	answer, err := pipeline.GenerateDraft(ctx, userID, msg)
	if err != nil {
		s.log.Error("slash command question: generation failed", "error", err)
		s.respondViaURL(responseURL, "Something went wrong. Please try again.")
		return
	}

	s.respondViaURL(responseURL, answer)
}

// respondViaURL sends an ephemeral response update via Slack's response_url.
func (s *apiServer) respondViaURL(responseURL, text string) {
	if responseURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"response_type":   "ephemeral",
		"replace_original": true,
		"text":            text,
	})
	resp, err := http.Post(responseURL, "application/json", bytes.NewReader(body))
	if err != nil {
		s.log.Error("slash command: failed to respond via URL", "error", err)
		return
	}
	resp.Body.Close()
}

// isDraftRequest returns true if the text appears to be a draft request
// rather than a question.
func isDraftRequest(text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range draftKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

