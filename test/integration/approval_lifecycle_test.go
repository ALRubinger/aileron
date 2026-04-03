//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestApprovalLifecycle_ApproveAndExecute(t *testing.T) {
	// Step 1: Create an intent that requires approval.
	intentBody := map[string]any{
		"workspace_id":   "default",
		"agent_id":       "test_agent",
		"idempotency_key": "lifecycle-test-1",
		"action": map[string]any{
			"type":    "git.pull_request.create",
			"summary": "Lifecycle test PR",
			"domain": map[string]any{
				"git": map[string]any{
					"provider":    "github",
					"repository":  "test/repo",
					"branch":      "feat/lifecycle",
					"base_branch": "main",
					"pr_title":    "Lifecycle test",
				},
			},
		},
	}

	intentResp := authedPost(t, apiURL()+"/v1/intents", intentBody)
	defer intentResp.Body.Close()

	if intentResp.StatusCode != http.StatusCreated {
		t.Fatalf("CreateIntent: expected 201, got %d", intentResp.StatusCode)
	}

	var intent map[string]any
	json.NewDecoder(intentResp.Body).Decode(&intent)

	intentID := intent["intent_id"].(string)
	decision := intent["decision"].(map[string]any)
	approvalID := decision["approval_id"].(string)

	if intent["status"] != "pending_approval" {
		t.Fatalf("intent status = %v, want pending_approval", intent["status"])
	}

	// Step 2: Get the approval.
	approvalResp := authedGet(t, apiURL()+"/v1/approvals/"+approvalID)
	defer approvalResp.Body.Close()

	if approvalResp.StatusCode != http.StatusOK {
		t.Fatalf("GetApproval: expected 200, got %d", approvalResp.StatusCode)
	}

	var approval map[string]any
	json.NewDecoder(approvalResp.Body).Decode(&approval)

	if approval["status"] != "pending" {
		t.Errorf("approval status = %v, want pending", approval["status"])
	}

	// Step 3: Approve the request.
	approveResp := authedPost(t, apiURL()+"/v1/approvals/"+approvalID+"/approve", map[string]any{"comment": "Looks good"})
	defer approveResp.Body.Close()

	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("Approve: expected 200, got %d", approveResp.StatusCode)
	}

	var approveResult map[string]any
	json.NewDecoder(approveResp.Body).Decode(&approveResult)

	if approveResult["status"] != "approved" {
		t.Errorf("approve result status = %v, want approved", approveResult["status"])
	}

	grantID, ok := approveResult["execution_grant_id"].(string)
	if !ok || grantID == "" {
		t.Fatal("expected execution_grant_id in approve result")
	}

	// Step 4: Verify intent status updated to approved.
	intentGetResp := authedGet(t, apiURL()+"/v1/intents/"+intentID)
	defer intentGetResp.Body.Close()

	var updatedIntent map[string]any
	json.NewDecoder(intentGetResp.Body).Decode(&updatedIntent)

	if updatedIntent["status"] != "approved" {
		t.Errorf("intent status after approval = %v, want approved", updatedIntent["status"])
	}

	// Step 5: Get the execution grant.
	grantResp := authedGet(t, apiURL()+"/v1/execution-grants/"+grantID)
	defer grantResp.Body.Close()

	if grantResp.StatusCode != http.StatusOK {
		t.Fatalf("GetGrant: expected 200, got %d", grantResp.StatusCode)
	}

	var grant map[string]any
	json.NewDecoder(grantResp.Body).Decode(&grant)

	if grant["status"] != "active" {
		t.Errorf("grant status = %v, want active", grant["status"])
	}
}

func TestApprovalLifecycle_Deny(t *testing.T) {
	// Create an intent that requires approval.
	intentBody := map[string]any{
		"workspace_id":   "default",
		"agent_id":       "test_agent",
		"idempotency_key": "lifecycle-deny-test-1",
		"action": map[string]any{
			"type":    "git.pull_request.create",
			"summary": "Deny test PR",
			"domain": map[string]any{
				"git": map[string]any{
					"repository":  "test/repo",
					"branch":      "feat/deny",
					"base_branch": "main",
					"pr_title":    "Should be denied",
				},
			},
		},
	}

	intentResp := authedPost(t, apiURL()+"/v1/intents", intentBody)
	defer intentResp.Body.Close()

	var intent map[string]any
	json.NewDecoder(intentResp.Body).Decode(&intent)

	decision := intent["decision"].(map[string]any)
	approvalID := decision["approval_id"].(string)
	intentID := intent["intent_id"].(string)

	// Deny the request.
	denyResp := authedPost(t, apiURL()+"/v1/approvals/"+approvalID+"/deny", map[string]any{"reason": "Not appropriate at this time"})
	defer denyResp.Body.Close()

	if denyResp.StatusCode != http.StatusOK {
		t.Fatalf("Deny: expected 200, got %d", denyResp.StatusCode)
	}

	var denyResult map[string]any
	json.NewDecoder(denyResp.Body).Decode(&denyResult)

	if denyResult["status"] != "denied" {
		t.Errorf("deny result status = %v, want denied", denyResult["status"])
	}
	if denyResult["intent_status"] != "denied" {
		t.Errorf("intent_status = %v, want denied", denyResult["intent_status"])
	}

	// Verify intent is denied.
	intentGetResp := authedGet(t, apiURL()+"/v1/intents/"+intentID)
	defer intentGetResp.Body.Close()

	var updatedIntent map[string]any
	json.NewDecoder(intentGetResp.Body).Decode(&updatedIntent)

	if updatedIntent["status"] != "denied" {
		t.Errorf("intent status after denial = %v, want denied", updatedIntent["status"])
	}
}
