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
	"strings"
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

// slackInnerEvent extracts the type from any Slack inner event.
type slackInnerEvent struct {
	Type string `json:"type"`
}

// slackMessageEvent is the inner event for message events.
type slackMessageEvent struct {
	Type        string `json:"type"`
	User        string `json:"user"`
	Text        string `json:"text"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type,omitempty"` // "im" for DMs, "channel"/"group" for channels
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	BotID       string `json:"bot_id,omitempty"`
}

// slackAssistantThreadEvent is the inner event for assistant_thread_started
// and assistant_thread_context_changed events.
type slackAssistantThreadEvent struct {
	Type            string `json:"type"`
	AssistantThread struct {
		ChannelID string `json:"channel_id"`
		ThreadTS  string `json:"thread_ts"`
		Context   struct {
			ChannelID string `json:"channel_id"`
			TeamID    string `json:"team_id"`
		} `json:"context"`
	} `json:"assistant_thread"`
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
// It routes events to the appropriate handler based on the inner event type.
func (s *apiServer) processSlackEvent(payload slackWebhookPayload) {
	// Peek at the event type to decide how to parse/route.
	var inner slackInnerEvent
	if err := json.Unmarshal(payload.Event, &inner); err != nil {
		s.log.Debug("slack webhook: failed to parse inner event type", "error", err)
		return
	}

	switch inner.Type {
	case "message":
		s.processSlackMessageEvent(payload)

	case "assistant_thread_started":
		var evt slackAssistantThreadEvent
		if err := json.Unmarshal(payload.Event, &evt); err != nil {
			s.log.Debug("slack webhook: failed to parse assistant_thread_started", "error", err)
			return
		}
		s.handleAssistantThreadStarted(
			context.Background(),
			payload.TeamID,
			evt.AssistantThread.ChannelID,
			evt.AssistantThread.ThreadTS,
		)

	case "assistant_thread_context_changed":
		var evt slackAssistantThreadEvent
		if err := json.Unmarshal(payload.Event, &evt); err != nil {
			s.log.Debug("slack webhook: failed to parse assistant_thread_context_changed", "error", err)
			return
		}
		s.log.Info("slack webhook: assistant_thread_context_changed",
			"channel_id", evt.AssistantThread.ChannelID,
			"thread_ts", evt.AssistantThread.ThreadTS,
			"context_channel", evt.AssistantThread.Context.ChannelID,
			"team_id", payload.TeamID,
		)

	default:
		s.log.Debug("slack webhook: unhandled event type", "type", inner.Type)
	}
}

// processSlackMessageEvent handles message events. It triggers draft generation
// only for Aileron users who are @mentioned in the message.
func (s *apiServer) processSlackMessageEvent(payload slackWebhookPayload) {
	var evt slackMessageEvent
	if err := json.Unmarshal(payload.Event, &evt); err != nil {
		s.log.Debug("slack webhook: failed to parse message event", "error", err)
		return
	}

	// Skip bot messages.
	if evt.BotID != "" {
		s.log.Debug("slack webhook: skipping bot message", "bot_id", evt.BotID)
		return
	}

	// Route DM messages (channel_type "im") to the agent handler.
	if evt.ChannelType == "im" {
		threadTS := evt.ThreadTS
		if threadTS == "" {
			threadTS = evt.TS
		}
		s.handleAssistantMessage(
			context.Background(),
			payload.TeamID,
			evt.Channel,
			threadTS,
			evt.User,
			evt.Text,
		)
		return
	}

	// Find all Aileron users connected to this Slack workspace.
	slackProvider := model.ConnectedAccountProviderSlack
	accounts, err := s.connectedAccounts.List(context.Background(), store.ConnectedAccountFilter{
		Provider:       &slackProvider,
		ExternalTeamID: payload.TeamID,
	})
	if err != nil {
		s.log.Error("slack webhook: failed to look up connected accounts", "error", err)
		return
	}
	if len(accounts) == 0 {
		s.log.Debug("slack webhook: no connected accounts for team", "team_id", payload.TeamID)
		return
	}

	msg := comms.BuildIncomingMessage(evt.TS, evt.Channel, evt.User, evt.Text)

	// Notify each mentioned user. If the author @mentions themselves,
	// they still get a draft — they explicitly asked for it.
	// Only skip the author when they are NOT mentioned.
	for _, acct := range accounts {
		if !isSlackMention(evt.Text, acct.ExternalUserID) {
			continue // only draft when the user is @mentioned
		}
		if s.onSlackMessage != nil {
			s.onSlackMessage(context.Background(), acct.UserID, msg)
		}
	}
}

// isSlackMention reports whether the Slack message text contains an @mention
// for the given user ID. Slack encodes mentions as <@UXXXXXX>.
func isSlackMention(text, slackUserID string) bool {
	return strings.Contains(text, "<@"+slackUserID+">")
}

// verifySlackSignature validates the HMAC-SHA256 signature from Slack.
func (s *apiServer) verifySlackSignature(r *http.Request, body []byte) bool {
	if s.slackSigningSecret == "" {
		s.log.Debug("slack signature: no signing secret configured")
		return false
	}

	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	signature := r.Header.Get("X-Slack-Signature")
	if timestamp == "" || signature == "" {
		s.log.Debug("slack signature: missing headers",
			"has_timestamp", timestamp != "",
			"has_signature", signature != "")
		return false
	}

	// Reject stale timestamps (>5 minutes) to prevent replay attacks.
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		s.log.Debug("slack signature: invalid timestamp", "timestamp", timestamp)
		return false
	}
	age := time.Now().Unix() - ts
	if math.Abs(float64(age)) > 300 {
		s.log.Debug("slack signature: stale timestamp",
			"timestamp", timestamp,
			"age_seconds", age)
		return false
	}

	// Compute expected signature: v0=HMAC-SHA256(signing_secret, "v0:{timestamp}:{body}")
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(s.slackSigningSecret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(signature)) {
		s.log.Debug("slack signature: mismatch",
			"expected_prefix", expected[:20],
			"received_prefix", signature[:min(len(signature), 20)],
			"body_length", len(body),
			"secret_length", len(s.slackSigningSecret))
		return false
	}

	return true
}

