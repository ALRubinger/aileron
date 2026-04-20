package model_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
)

// TestUserInstruction_JSONKeys verifies snake_case JSON serialization.
func TestUserInstruction_JSONKeys(t *testing.T) {
	inst := model.UserInstruction{
		ID:        "ins_123",
		UserID:    "usr_456",
		Body:      "Always be concise",
		Scope:     "global",
		Active:    true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}

	b, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	required := []string{"id", "user_id", "body", "scope", "active", "created_at", "updated_at"}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("missing expected key %q in JSON output", key)
		}
	}

	// Verify no PascalCase keys leaked through.
	banned := []string{"ID", "UserID", "Body", "Scope", "Active", "CreatedAt", "UpdatedAt"}
	for _, key := range banned {
		if _, ok := m[key]; ok {
			t.Errorf("unexpected PascalCase key %q in JSON output", key)
		}
	}
}

// TestDraft_JSONKeys verifies snake_case JSON serialization.
func TestDraft_JSONKeys(t *testing.T) {
	draft := model.Draft{
		ID:          "dft_123",
		UserID:      "usr_456",
		Status:      model.DraftStatusPending,
		Service:     "slack",
		Channel:     "C123",
		Author:      "sarah",
		MessageBody: "original message",
		MessageTS:   "1234567890.123456",
		DraftBody:   "draft reply",
		SentBody:    "",
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}

	b, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	required := []string{"id", "user_id", "status", "service", "channel", "author",
		"message_body", "message_ts", "draft_body", "sent_body", "created_at", "updated_at"}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("missing expected key %q in JSON output", key)
		}
	}

	banned := []string{"MessageBody", "MessageTS", "DraftBody", "SentBody"}
	for _, key := range banned {
		if _, ok := m[key]; ok {
			t.Errorf("unexpected PascalCase key %q in JSON output", key)
		}
	}
}

// TestDraftFeedback_JSONKeys verifies snake_case JSON serialization.
func TestDraftFeedback_JSONKeys(t *testing.T) {
	fb := model.DraftFeedback{
		ID:        "fb_123",
		UserID:    "usr_456",
		DraftID:   "dft_789",
		Signal:    model.FeedbackSignalApproved,
		Service:   "slack",
		Channel:   "C123",
		DraftBody: "draft text",
		SentBody:  "sent text",
		ToolsUsed: []string{"gmail_search"},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	b, err := json.Marshal(fb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	required := []string{"id", "user_id", "draft_id", "signal", "service", "channel",
		"draft_body", "sent_body", "tools_used", "created_at"}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("missing expected key %q in JSON output", key)
		}
	}

	banned := []string{"DraftID", "DraftBody", "SentBody", "ToolsUsed"}
	for _, key := range banned {
		if _, ok := m[key]; ok {
			t.Errorf("unexpected PascalCase key %q in JSON output", key)
		}
	}
}

// TestUser_JSONKeys verifies sensitive fields are excluded from JSON.
func TestUser_JSONKeys(t *testing.T) {
	user := model.User{
		ID:           "usr_123",
		EnterpriseID: "ent_456",
		Email:        "test@example.com",
		PasswordHash: "secret-hash",
	}

	b, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// PasswordHash must be excluded via json:"-".
	if _, ok := m["password_hash"]; ok {
		t.Error("password_hash should not appear in JSON output")
	}
	if _, ok := m["PasswordHash"]; ok {
		t.Error("PasswordHash should not appear in JSON output")
	}

	// Other fields should be present with snake_case keys.
	if _, ok := m["enterprise_id"]; !ok {
		t.Error("expected enterprise_id in JSON output")
	}
}
