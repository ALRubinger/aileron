package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newCommandTestServer() *apiServer {
	return &apiServer{
		log:                slog.Default(),
		connectedAccounts:  mem.NewConnectedAccountStore(),
		systemVault:        vault.NewMemVault(),
		vault:              vault.NewMemVault(),
		slackSigningSecret: testSigningSecret,
		newID:              func() string { return "test-id" },
	}
}

func signedCommandRequest(params url.Values) *http.Request {
	body := params.Encode()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, body)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/commands",
		strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)
	return r
}

func TestSlackCommand_MethodNotAllowed(t *testing.T) {
	srv := newCommandTestServer()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/webhooks/slack/commands", nil)
	srv.handleSlackCommand(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestSlackCommand_InvalidSignature(t *testing.T) {
	srv := newCommandTestServer()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/webhooks/slack/commands",
		strings.NewReader("text=hello"))
	r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-Slack-Signature", "v0=invalid")
	srv.handleSlackCommand(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestSlackCommand_Question_Returns200(t *testing.T) {
	srv := newCommandTestServer()
	params := url.Values{
		"command":      {"/aileron"},
		"text":         {"How many hours on calls today?"},
		"team_id":      {"T001"},
		"channel_id":   {"C123"},
		"user_id":      {"U_ALICE"},
		"response_url": {"https://hooks.slack.com/commands/test"},
	}

	w := httptest.NewRecorder()
	srv.handleSlackCommand(w, signedCommandRequest(params))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["response_type"] != "ephemeral" {
		t.Errorf("expected ephemeral response, got %v", resp["response_type"])
	}
}

func TestSlackCommand_Draft_Returns200(t *testing.T) {
	srv := newCommandTestServer()
	params := url.Values{
		"command":    {"/aileron"},
		"text":       {"Draft me a weekly status update"},
		"team_id":    {"T001"},
		"channel_id": {"C123"},
		"user_id":    {"U_ALICE"},
		"trigger_id": {"trig_123"},
	}

	w := httptest.NewRecorder()
	srv.handleSlackCommand(w, signedCommandRequest(params))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// The draft flow opens a modal async — response is empty 200.
	time.Sleep(50 * time.Millisecond)
}
