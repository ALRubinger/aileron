package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	connectorpkg "github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/policy"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
)

func newInteractionTestServer() *apiServer {
	srv := &apiServer{
		log:                slog.Default(),
		drafts:             mem.NewDraftStore(),
		feedback:           mem.NewDraftFeedbackStore(),
		connectedAccounts:  mem.NewConnectedAccountStore(),
		vault:              vault.NewMemVault(),
		slackSigningSecret: testSigningSecret,
		users:              &stubUserStore{},
		newID:              func() string { return "test-id" },
	}
	srv.slackSender = func(_ context.Context, _, _, _, _ string) error { return nil }
	return srv
}

func signedInteractionRequest(payloadJSON string) (*http.Request, string) {
	formData := url.Values{"payload": {payloadJSON}}.Encode()
	body := []byte(formData)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/interactions",
		strings.NewReader(formData))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)
	return r, ts
}

func newMockResponseURLServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestInteraction_InvalidSignature(t *testing.T) {
	srv := newInteractionTestServer()

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/interactions",
		strings.NewReader("payload={}"))
	r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-Slack-Signature", "v0=invalid")

	w := httptest.NewRecorder()
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestInteraction_MethodNotAllowed(t *testing.T) {
	srv := newInteractionTestServer()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/webhooks/slack/interactions", nil)
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestInteraction_MissingPayload(t *testing.T) {
	srv := newInteractionTestServer()
	body := []byte("other=data")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/interactions",
		strings.NewReader(string(body)))
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)

	w := httptest.NewRecorder()
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestInteraction_NonBlockActions(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-block_actions, got %d", w.Code)
	}
}

func TestInteraction_RespondToInteraction_EmptyURL(t *testing.T) {
	srv := newInteractionTestServer()
	// Should not panic with empty response URL.
	srv.respondToInteraction("", "test message")
}

func TestInteraction_MessageAction_Parsed(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type":        "message_action",
		"callback_id": "draft_reply",
		"trigger_id":  "trig_12345",
		"user":        map[string]any{"id": "U_ALICE"},
		"team":        map[string]any{"id": "T001TEAM"},
		"channel":     map[string]any{"id": "C0BACKEND", "name": "backend"},
		"message": map[string]any{
			"text":      "Can someone review PR #42?",
			"user":      "U_BOB",
			"ts":        "111.222",
			"thread_ts": "111.000",
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Currently a stub — no panic, correct routing = pass.
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_MessageAction_NoActions(t *testing.T) {
	// message_action payloads have no actions array — ensure we don't crash.
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type":        "message_action",
		"callback_id": "draft_reply",
		"trigger_id":  "trig_999",
		"user":        map[string]any{"id": "U_ALICE"},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_ViewSubmission_SendsDraft(t *testing.T) {
	srv := newInteractionTestServer()
	srv.systemVault = vault.NewMemVault()
	enableVaultEncryption(srv, "usr_a")
	ctx := context.Background()

	// Seed connected account + encrypted user token.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})
	storeEncryptedToken(srv, "connected-accounts/usr_a/slack",
		[]byte(`{"access_token":"xoxp-test"}`))

	var mu sync.Mutex
	var sentBody string
	srv.slackSender = func(_ context.Context, _, _, body, _ string) error {
		mu.Lock()
		sentBody = body
		mu.Unlock()
		return nil
	}

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:  "C0BACKEND",
		TargetThreadTS: "111.222",
		UserID:         "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{
							"value": "This is my edited draft",
						},
					},
				},
			},
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := sentBody
	mu.Unlock()
	if got != "This is my edited draft" {
		t.Errorf("expected draft text to be sent, got %q", got)
	}
}

func TestInteraction_ViewSubmission_WrongCallback(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"view": map[string]any{
			"id":          "V_123",
			"callback_id": "some_other_modal",
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Should be ignored — no crash.
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_ViewSubmission_EmptyDraft(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel: "C0BACKEND",
		UserID:        "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{
							"value": "", // empty draft
						},
					},
				},
			},
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	// No crash, empty draft is logged and ignored.
}

func TestInteraction_ViewSubmission_NoState(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel: "C0BACKEND",
		UserID:        "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			// no state field
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_SendDraftAgent(t *testing.T) {
	srv := newInteractionTestServer()
	enableVaultEncryption(srv, "usr_a")
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})
	storeEncryptedToken(srv, "connected-accounts/usr_a/slack",
		[]byte(`{"access_token":"xoxp-test"}`))

	var mu sync.Mutex
	var sentBody, sentChannel string
	srv.slackSender = func(_ context.Context, _, channel, body, _ string) error {
		mu.Lock()
		sentBody = body
		sentChannel = channel
		mu.Unlock()
		return nil
	}

	sendMeta, _ := json.Marshal(map[string]any{
		"channel":   "C_TARGET",
		"thread_ts": "111.222",
		"body":      "Draft from agent DM",
		"user_id":   "usr_a",
	})

	// Create a pending draft so processInteraction doesn't bail on draft lookup.
	srv.drafts.Create(ctx, model.Draft{
		ID: string(sendMeta), UserID: "usr_a", Status: model.DraftStatusPending,
	})

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "send_draft_agent", "value": string(sendMeta)}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if sentBody != "Draft from agent DM" {
		t.Errorf("expected 'Draft from agent DM', got %q", sentBody)
	}
	if sentChannel != "C_TARGET" {
		t.Errorf("expected C_TARGET, got %q", sentChannel)
	}
}

func TestInteraction_SendDraftAgent_BadJSON(t *testing.T) {
	srv := newInteractionTestServer()

	// Create a draft with ID matching the bad JSON so processInteraction finds it.
	srv.drafts.Create(context.Background(), model.Draft{
		ID: "not-valid-json", UserID: "usr_a", Status: model.DraftStatusPending,
	})

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "send_draft_agent", "value": "not-valid-json"}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	// No crash — bad JSON logged and returned.
}

func TestInteraction_RefineAction_RoutedCorrectly(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:   "C0BACKEND",
		OriginalMessage: "Original msg",
		UserID:          "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{
							"value": "Current draft text",
						},
					},
					instructionBlockID: map[string]any{
						instructionActionID: map[string]any{
							"value": "Make it shorter",
						},
					},
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// processRefineDraft runs async; it will fail on bot token resolution
	// but the key assertion is that routing works and doesn't crash.
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_RefineAction_NoView(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
		// No view — processRefineDraft should log and return.
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_RefineAction_EmptyDraft(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel: "C0BACKEND",
		UserID:        "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{
							"value": "", // empty draft
						},
					},
					instructionBlockID: map[string]any{
						instructionActionID: map[string]any{
							"value": "Some feedback",
						},
					},
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_RefineAction_NoFeedback(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel: "C0BACKEND",
		UserID:        "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{
							"value": "Some draft",
						},
					},
					// No feedback block
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_RespondToInteraction_Success(t *testing.T) {
	var receivedBody string
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if text, ok := payload["text"].(string); ok {
			receivedBody = text
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

	srv := newInteractionTestServer()
	srv.respondToInteraction(respServer.URL, "Draft sent!")

	if receivedBody != "Draft sent!" {
		t.Fatalf("expected 'Draft sent!', got %q", receivedBody)
	}
}

func TestInteraction_RespondToInteraction_ServerError(t *testing.T) {
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer respServer.Close()

	srv := newInteractionTestServer()
	// Should not panic on error response.
	srv.respondToInteraction(respServer.URL, "test message")
}

func TestInteraction_RefineAction_NoPipeline(t *testing.T) {
	srv := newInteractionTestServer()
	srv.systemVault = vault.NewMemVault()

	// Store a bot token so resolveWorkspaceBotToken succeeds.
	srv.systemVault.Put(context.Background(), "slack-workspaces/T001/bot-token",
		[]byte("xoxb-test-bot-token"), vault.Metadata{Type: "bot_token"})

	// Create connected account so resolveAileronUserBySlack succeeds.
	srv.connectedAccounts.Create(context.Background(), model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})

	// No draftPipeline — should hit the "not configured" branch.
	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:   "C0BACKEND",
		OriginalMessage: "Original msg",
		UserID:          "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{"value": "Draft text"},
					},
					instructionBlockID: map[string]any{
						instructionActionID: map[string]any{"value": "Make shorter"},
					},
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestInteraction_RefineAction_UserNotFound(t *testing.T) {
	srv := newInteractionTestServer()
	srv.systemVault = vault.NewMemVault()

	srv.systemVault.Put(context.Background(), "slack-workspaces/T001/bot-token",
		[]byte("xoxb-test-bot-token"), vault.Metadata{Type: "bot_token"})

	// No connected account — resolveAileronUserBySlack will fail.
	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:   "C0BACKEND",
		OriginalMessage: "Original msg",
		UserID:          "U_UNKNOWN",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_UNKNOWN"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{"value": "Draft text"},
					},
					instructionBlockID: map[string]any{
						instructionActionID: map[string]any{"value": "Feedback"},
					},
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestInteraction_RefineAction_VaultLocked(t *testing.T) {
	srv := newInteractionTestServer()
	srv.systemVault = vault.NewMemVault()
	srv.draftPipeline = newTestPipeline("context", "Draft reply")

	// Bot token present.
	srv.systemVault.Put(context.Background(), "slack-workspaces/T001/bot-token",
		[]byte("xoxb-test-bot-token"), vault.Metadata{Type: "bot_token"})

	// Set kekSessionCache so resolvePipelineVault doesn't fall through to
	// tier 3 (no-auth local dev). Without a KEK for this user, it hits vault-locked.
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)

	srv.connectedAccounts.Create(context.Background(), model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:   "C0BACKEND",
		OriginalMessage: "Original msg",
		UserID:          "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{"value": "My draft"},
					},
					instructionBlockID: map[string]any{
						instructionActionID: map[string]any{"value": "Make shorter"},
					},
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// processRefineDraft should hit the vaultLockedSlackMessage branch.
	time.Sleep(100 * time.Millisecond)
}

func TestInteraction_RefineAction_FullPipeline(t *testing.T) {
	srv := newInteractionTestServer()
	srv.systemVault = vault.NewMemVault()
	srv.draftPipeline = newTestPipeline("context", "Refined draft text")

	// Bot token.
	srv.systemVault.Put(context.Background(), "slack-workspaces/T001/bot-token",
		[]byte("xoxb-test-bot-token"), vault.Metadata{Type: "bot_token"})

	// Connected account.
	srv.connectedAccounts.Create(context.Background(), model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})

	// Unlock vault so resolvePipelineVault returns a pipeline (tier 3: no auth).
	// kekSessionCache=nil signals local dev mode where vault is not required.

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:   "C0BACKEND",
		OriginalMessage: "Can you review this PR?",
		UserID:          "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{"value": "Here is my draft review"},
					},
					instructionBlockID: map[string]any{
						instructionActionID: map[string]any{"value": "Make it more professional"},
					},
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Exercises L202 (updateDraftModalLoading), L221 (RefineDraft), L228 (updateDraftModalWithDraft).
	time.Sleep(200 * time.Millisecond)
}

func TestInteraction_RefineAction_PipelineError(t *testing.T) {
	srv := newInteractionTestServer()
	srv.systemVault = vault.NewMemVault()

	// Pipeline that always errors.
	errClient := &mockLLMClient{err: fmt.Errorf("LLM down")}
	srv.draftPipeline = draft.NewPipeline(
		errClient, errClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "r", Ghostwrite: "g"},
	)

	srv.systemVault.Put(context.Background(), "slack-workspaces/T001/bot-token",
		[]byte("xoxb-test-bot-token"), vault.Metadata{Type: "bot_token"})
	srv.connectedAccounts.Create(context.Background(), model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:   "C0BACKEND",
		OriginalMessage: "Original msg",
		UserID:          "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{"value": "My draft"},
					},
					instructionBlockID: map[string]any{
						instructionActionID: map[string]any{"value": "Feedback"},
					},
				},
			},
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Exercises L221-226 (pipeline error → updateDraftModalError).
	time.Sleep(200 * time.Millisecond)
}

func TestInteraction_RefineAction_BadMetadata(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U_ALICE"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": "not-valid-json",
		},
		"actions": []map[string]any{{
			"action_id": refineActionID,
			"value":     "refine",
		}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_InstructionsSubmission_ReturnsUpdateAction(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:   "C0BACKEND",
		TargetThreadTS:  "111.222",
		OriginalMessage: "Can someone review PR #42?",
		UserID:          "U_ALICE",
		TeamID:          "T001",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_INSTR_123",
			"callback_id":      instructionsModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					instructionsInputBlockID: map[string]any{
						instructionsInputActionID: map[string]any{
							"value": "Politely decline and suggest next week",
						},
					},
				},
			},
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify the response contains a response_action: "update" to keep the modal open.
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["response_action"] != "update" {
		t.Errorf("expected response_action 'update', got %v", resp["response_action"])
	}

	// Verify the updated view has the draft modal callback ID (loading state).
	viewMap, ok := resp["view"].(map[string]any)
	if !ok {
		t.Fatal("expected view in response")
	}
	if viewMap["callback_id"] != draftModalCallbackID {
		t.Errorf("expected callback_id %q, got %v", draftModalCallbackID, viewMap["callback_id"])
	}

	// The async generateDraftFromInstructions will fail (no bot token) but
	// the key assertion is that the response_action flow works correctly.
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_InstructionsSubmission_BadMetadata(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"view": map[string]any{
			"id":               "V_INSTR_123",
			"callback_id":      instructionsModalCallbackID,
			"private_metadata": "not-valid-json",
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Bad metadata — should return 200 without crashing.
}

func TestInteraction_CancelDraft(t *testing.T) {
	srv := newInteractionTestServer()

	// Set up a response URL server to capture the response.
	var mu sync.Mutex
	var responseBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		responseBody = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	payload, _ := json.Marshal(map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": "U_ALICE"},
		"response_url": responseServer.URL,
		"actions":      []map[string]any{{"action_id": "cancel_draft", "value": ""}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for async handler.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	body := responseBody
	mu.Unlock()
	if !strings.Contains(body, "Cancelled") {
		t.Errorf("expected 'Cancelled' in response, got %q", body)
	}
}

func TestInteraction_CancelDraft_NoResponseURL(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "cancel_draft", "value": ""}},
		// no response_url
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Should not panic with empty response URL.
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_UnknownAction(t *testing.T) {
	srv := newInteractionTestServer()

	srv.drafts.Create(context.Background(), model.Draft{
		ID: "dft_1", UserID: "usr_a", Status: model.DraftStatusPending,
	})

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "some_unknown_action", "value": "dft_1"}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

// --- processApproveIntent tests ---

func TestInteraction_ApproveIntent_InvalidJSON(t *testing.T) {
	srv := newInteractionTestServer()
	srv.intents = mem.NewIntentStore()
	srv.grants = mem.NewGrantStore()
	srv.executions = mem.NewExecutionStore()
	srv.traces = mem.NewTraceStore()
	srv.registry = connectorpkg.NewRegistry()
	srv.policyEngine = policy.NewRuleEngine(mem.NewPolicyStore())

	var mu sync.Mutex
	var body string
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions", "user": map[string]any{"id": "U_ALICE"},
		"response_url": respServer.URL,
		"actions":      []map[string]any{{"action_id": "approve_intent", "value": "not-json"}},
	})
	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := body
	mu.Unlock()
	if !strings.Contains(got, "Failed to process approval") {
		t.Errorf("expected error response, got: %s", got)
	}
}

func TestInteraction_ApproveIntent_NoAction(t *testing.T) {
	srv := newInteractionTestServer()
	srv.intents = mem.NewIntentStore()
	srv.grants = mem.NewGrantStore()
	srv.executions = mem.NewExecutionStore()
	srv.traces = mem.NewTraceStore()
	srv.registry = connectorpkg.NewRegistry()
	srv.policyEngine = policy.NewRuleEngine(mem.NewPolicyStore())

	var mu sync.Mutex
	var body string
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

	actionValue, _ := json.Marshal(map[string]any{"type": "email.send", "user_id": "usr_a"})
	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions", "user": map[string]any{"id": "U_ALICE"},
		"response_url": respServer.URL,
		"actions":      []map[string]any{{"action_id": "approve_intent", "value": string(actionValue)}},
	})
	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := body
	mu.Unlock()
	if !strings.Contains(got, "No action to execute") {
		t.Errorf("expected 'No action to execute', got: %s", got)
	}
}

func TestInteraction_ApproveIntent_CreatesIntentAndGrant(t *testing.T) {
	srv := newInteractionTestServer()
	srv.intents = mem.NewIntentStore()
	srv.grants = mem.NewGrantStore()
	srv.executions = mem.NewExecutionStore()
	srv.traces = mem.NewTraceStore()
	srv.registry = connectorpkg.NewRegistry()
	srv.policyEngine = policy.NewRuleEngine(mem.NewPolicyStore())

	actionValue, _ := json.Marshal(map[string]any{
		"type": "git.issue.create", "user_id": "usr_a",
		"action": map[string]any{
			"type": "git.issue.create", "summary": "Create bug report",
		},
	})
	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions", "user": map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "approve_intent", "value": string(actionValue)}},
	})
	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)
	time.Sleep(200 * time.Millisecond)

	ctx := context.Background()
	intent, err := srv.intents.Get(ctx, "int_test-id")
	if err != nil {
		t.Fatalf("expected intent in store: %v", err)
	}
	// Intent progresses through approved → executing → completed/failed.
	// We verify it was created and processed (not still pending_policy).
	if intent.Status == api.PendingPolicy {
		t.Errorf("intent should have been processed, still %q", intent.Status)
	}
	grant, err := srv.grants.Get(ctx, "grt_test-id")
	if err != nil {
		t.Fatalf("expected grant in store: %v", err)
	}
	if grant.IntentId != "int_test-id" {
		t.Errorf("grant intent_id = %q", grant.IntentId)
	}
}
