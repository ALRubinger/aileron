package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// slackEventDedup is a simple in-memory deduplication cache for Slack event IDs.
// Events are retained for 5 minutes to handle Slack's retry behavior.
type slackEventDedup struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	ttl   time.Duration
}

func newSlackEventDedup() *slackEventDedup {
	return &slackEventDedup{
		seen: make(map[string]time.Time),
		ttl:  5 * time.Minute,
	}
}

// isDuplicate returns true if the event ID has been seen within the TTL.
// If not a duplicate, it records the event ID.
func (d *slackEventDedup) isDuplicate(eventID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Lazy sweep: remove expired entries.
	now := time.Now()
	for id, ts := range d.seen {
		if now.Sub(ts) > d.ttl {
			delete(d.seen, id)
		}
	}

	if _, ok := d.seen[eventID]; ok {
		return true
	}
	d.seen[eventID] = now
	return false
}

// slackWebhookPayload is the top-level structure of a Slack Events API POST.
type slackWebhookPayload struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	Challenge string `json:"challenge,omitempty"` // url_verification only
	TeamID    string `json:"team_id,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
}

// slackMessageEvent is the inner event for message events.
type slackMessageEvent struct {
	Type    string `json:"type"`
	User    string `json:"user"`
	Text    string `json:"text"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	BotID   string `json:"bot_id,omitempty"`
}

// handleSlackEvent handles POST /v1/webhooks/slack/events.
// It verifies the Slack request signature, handles url_verification challenges,
// and routes message events to the appropriate Aileron user.
func (s *apiServer) handleSlackEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Verify Slack request signature.
	if !s.verifySlackSignature(r, body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var payload slackWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	switch payload.Type {
	case "url_verification":
		// Slack sends this once during app setup to verify the endpoint.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": payload.Challenge})
		return

	case "event_callback":
		// Respond 200 immediately — Slack requires this within 3 seconds.
		w.WriteHeader(http.StatusOK)

		// Deduplicate.
		if s.slackDedup != nil && s.slackDedup.isDuplicate(payload.EventID) {
			s.log.Debug("slack webhook: duplicate event, skipping", "event_id", payload.EventID)
			return
		}

		// Process async.
		go s.processSlackEvent(payload)
		return

	default:
		s.log.Debug("slack webhook: unknown payload type", "type", payload.Type)
		w.WriteHeader(http.StatusOK)
	}
}

// processSlackEvent handles a Slack event_callback payload asynchronously.
func (s *apiServer) processSlackEvent(payload slackWebhookPayload) {
	var evt slackMessageEvent
	if err := json.Unmarshal(payload.Event, &evt); err != nil {
		s.log.Debug("slack webhook: failed to parse inner event", "error", err)
		return
	}

	if evt.Type != "message" {
		s.log.Debug("slack webhook: non-message event, ignoring", "type", evt.Type)
		return
	}

	// Skip bot messages.
	if evt.BotID != "" {
		s.log.Debug("slack webhook: skipping bot message", "bot_id", evt.BotID)
		return
	}

	// Look up the Aileron user by Slack team + user ID.
	slackProvider := model.ConnectedAccountProviderSlack
	accounts, err := s.connectedAccounts.List(context.Background(), store.ConnectedAccountFilter{
		Provider:       &slackProvider,
		ExternalTeamID: payload.TeamID,
		ExternalUserID: evt.User,
	})
	if err != nil {
		s.log.Error("slack webhook: failed to look up connected account", "error", err)
		return
	}
	if len(accounts) == 0 {
		s.log.Debug("slack webhook: no connected account for event",
			"team_id", payload.TeamID, "user_id", evt.User)
		return
	}

	acct := accounts[0]
	msg := comms.BuildIncomingMessage(evt.TS, evt.Channel, evt.User, evt.Text)

	if s.onSlackMessage != nil {
		s.onSlackMessage(context.Background(), acct.UserID, msg)
	} else {
		s.log.Info("slack webhook: message received (no handler configured)",
			"user_id", acct.UserID, "channel", evt.Channel)
	}
}

// verifySlackSignature validates the HMAC-SHA256 signature from Slack.
func (s *apiServer) verifySlackSignature(r *http.Request, body []byte) bool {
	if s.slackSigningSecret == "" {
		return false
	}

	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	signature := r.Header.Get("X-Slack-Signature")
	if timestamp == "" || signature == "" {
		return false
	}

	// Reject stale timestamps (>5 minutes) to prevent replay attacks.
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if math.Abs(float64(time.Now().Unix()-ts)) > 300 {
		return false
	}

	// Compute expected signature: v0=HMAC-SHA256(signing_secret, "v0:{timestamp}:{body}")
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(s.slackSigningSecret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

