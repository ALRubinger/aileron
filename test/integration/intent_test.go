//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCreateIntent_AutoApproveFeatureBranch(t *testing.T) {
	body := map[string]any{
		"workspace_id":   "default",
		"agent_id":       "claude_code",
		"idempotency_key": "test-auto-approve-1",
		"action": map[string]any{
			"type":    "git.pull_request.create",
			"summary": "Add tests for auth module",
			"domain": map[string]any{
				"git": map[string]any{
					"provider":    "github",
					"repository":  "acme/checkout",
					"branch":      "feat/add-tests",
					"base_branch": "develop",
					"pr_title":    "Add tests for auth module",
				},
			},
		},
		"context": map[string]any{
			"source_platform": "claude_code",
			"user_present":    true,
		},
	}

	resp := postJSON(t, apiURL()+"/v1/intents", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if result["intent_id"] == nil {
		t.Fatal("expected intent_id in response")
	}
	if result["status"] != "approved" {
		t.Errorf("status = %v, want approved", result["status"])
	}

	decision, _ := result["decision"].(map[string]any)
	if decision["disposition"] != "allow" {
		t.Errorf("disposition = %v, want allow", decision["disposition"])
	}
	if decision["execution_grant_id"] == nil {
		t.Error("expected execution_grant_id for auto-approved intent")
	}
}

func TestCreateIntent_RequireApprovalForMain(t *testing.T) {
	body := map[string]any{
		"workspace_id":   "default",
		"agent_id":       "claude_code",
		"idempotency_key": "test-require-approval-1",
		"action": map[string]any{
			"type":    "git.pull_request.create",
			"summary": "Fix tax rounding bug",
			"domain": map[string]any{
				"git": map[string]any{
					"provider":    "github",
					"repository":  "acme/checkout",
					"branch":      "fix/tax-rounding",
					"base_branch": "main",
					"pr_title":    "Fix tax rounding",
				},
			},
		},
	}

	resp := postJSON(t, apiURL()+"/v1/intents", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if result["status"] != "pending_approval" {
		t.Errorf("status = %v, want pending_approval", result["status"])
	}

	decision, _ := result["decision"].(map[string]any)
	if decision["disposition"] != "require_approval" {
		t.Errorf("disposition = %v, want require_approval", decision["disposition"])
	}
	if decision["approval_id"] == nil {
		t.Error("expected approval_id in decision")
	}
	if decision["requires_approval"] != true {
		t.Errorf("requires_approval = %v, want true", decision["requires_approval"])
	}
}

func TestCreateIntent_DenyForcePush(t *testing.T) {
	body := map[string]any{
		"workspace_id":   "default",
		"agent_id":       "claude_code",
		"idempotency_key": "test-deny-force-push-1",
		"action": map[string]any{
			"type":    "git.force_push",
			"summary": "Force push to main",
		},
	}

	resp := postJSON(t, apiURL()+"/v1/intents", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if result["status"] != "denied" {
		t.Errorf("status = %v, want denied", result["status"])
	}

	decision, _ := result["decision"].(map[string]any)
	if decision["disposition"] != "deny" {
		t.Errorf("disposition = %v, want deny", decision["disposition"])
	}
	if decision["denial_reason"] == nil || decision["denial_reason"] == "" {
		t.Error("expected denial_reason")
	}
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}
