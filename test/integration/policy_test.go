//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestListPolicies_SeedPoliciesExist(t *testing.T) {
	resp, err := http.Get(apiURL() + "/v1/policies?workspace_id=default")
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	items, ok := result["items"].([]any)
	if !ok {
		t.Fatal("expected items array in response")
	}
	if len(items) < 3 {
		t.Errorf("expected at least 3 seed policies, got %d", len(items))
	}

	// Check that expected policy names exist.
	names := map[string]bool{}
	for _, item := range items {
		p := item.(map[string]any)
		names[p["name"].(string)] = true
	}

	expectedNames := []string{
		"Require approval for PRs to protected branches",
		"Allow PRs to feature branches",
		"Deny force pushes",
	}
	for _, name := range expectedNames {
		if !names[name] {
			t.Errorf("missing seed policy: %q", name)
		}
	}
}

func TestSimulatePolicy(t *testing.T) {
	body := map[string]any{
		"workspace_id": "default",
		"action": map[string]any{
			"type":    "git.pull_request.create",
			"summary": "Test simulation",
			"domain": map[string]any{
				"git": map[string]any{
					"repository":  "test/repo",
					"branch":      "feat/sim",
					"base_branch": "main",
					"pr_title":    "Simulate me",
				},
			},
		},
	}

	resp := postJSON(t, apiURL()+"/v1/policies/simulate", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	decision, _ := result["decision"].(map[string]any)
	if decision["disposition"] != "require_approval" {
		t.Errorf("disposition = %v, want require_approval", decision["disposition"])
	}
}

func TestCreatePolicy(t *testing.T) {
	body := map[string]any{
		"workspace_id": "default",
		"name":         "Test Custom Policy",
		"description":  "A test policy created via API",
		"rules": []any{
			map[string]any{
				"rule_id":     "custom_rule_1",
				"effect":      "deny",
				"description": "Deny all deployments",
				"priority":    300,
				"conditions": []any{
					map[string]any{
						"field":    "action.type",
						"operator": "eq",
						"value":    "deploy.create",
					},
				},
			},
		},
	}

	resp := postJSON(t, apiURL()+"/v1/policies", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if result["policy_id"] == nil {
		t.Error("expected policy_id")
	}
	if result["name"] != "Test Custom Policy" {
		t.Errorf("name = %v, want Test Custom Policy", result["name"])
	}
	if result["status"] != "active" {
		t.Errorf("status = %v, want active", result["status"])
	}
}
