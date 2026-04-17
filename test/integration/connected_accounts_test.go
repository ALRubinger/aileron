//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestConnectedAccounts_CRUD(t *testing.T) {
	// Create a connected account directly.
	resp := authedPost(t, apiURL()+"/v1/connected-accounts", map[string]any{
		"provider":         "slack",
		"email":            "",
		"scopes":           []string{"channels:history", "chat:write"},
		"external_user_id": "U_INTEGRATION_TEST",
		"external_team_id": "T_INTEGRATION_TEST",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: expected 201, got %d: %s", resp.StatusCode, body)
	}

	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	acctID := stringField(created, "id")
	if acctID == "" {
		t.Fatalf("expected account ID in response, got: %v", created)
	}
	t.Logf("created connected account: %s", acctID)

	// List — should contain the one we created.
	listResp := authedGet(t, apiURL()+"/v1/connected-accounts")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("list: expected 200, got %d: %s", listResp.StatusCode, body)
	}

	var listResult map[string]any
	json.NewDecoder(listResp.Body).Decode(&listResult)
	items, _ := listResult["items"].([]any)
	found := false
	for _, item := range items {
		m, _ := item.(map[string]any)
		if stringField(m, "id") == acctID {
			found = true
		}
	}
	if !found {
		t.Fatalf("list: created account %s not found in results", acctID)
	}

	// Get the specific account.
	getResp := authedGet(t, apiURL()+"/v1/connected-accounts/"+acctID)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("get: expected 200, got %d: %s", getResp.StatusCode, body)
	}

	// Delete the account.
	deleteResp := authedDelete(t, apiURL()+"/v1/connected-accounts/"+acctID)
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete: expected 204, got %d: %s", deleteResp.StatusCode, body)
	}

	// Verify deleted.
	getAfterDelete := authedGet(t, apiURL()+"/v1/connected-accounts/"+acctID)
	defer getAfterDelete.Body.Close()
	if getAfterDelete.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", getAfterDelete.StatusCode)
	}
}

func TestConnectedAccounts_CreateAndListWithFilters(t *testing.T) {
	// Create two accounts with different providers.
	resp1 := authedPost(t, apiURL()+"/v1/connected-accounts", map[string]any{
		"provider":         "slack",
		"scopes":           []string{"channels:history"},
		"external_user_id": "U_FILTER_TEST_1",
		"external_team_id": "T_FILTER_TEST",
	})
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("create slack: expected 201, got %d: %s", resp1.StatusCode, body)
	}
	var created1 map[string]any
	json.NewDecoder(resp1.Body).Decode(&created1)
	id1 := stringField(created1, "id")

	resp2 := authedPost(t, apiURL()+"/v1/connected-accounts", map[string]any{
		"provider":         "github_repos",
		"email":            "test@github.com",
		"scopes":           []string{"repo"},
		"external_user_id": "ghuser",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("create github: expected 201, got %d: %s", resp2.StatusCode, body)
	}
	var created2 map[string]any
	json.NewDecoder(resp2.Body).Decode(&created2)
	id2 := stringField(created2, "id")

	// List all — should have at least 2.
	listAll := authedGet(t, apiURL()+"/v1/connected-accounts")
	defer listAll.Body.Close()
	var allResult map[string]any
	json.NewDecoder(listAll.Body).Decode(&allResult)
	items, _ := allResult["items"].([]any)
	if len(items) < 2 {
		t.Fatalf("expected at least 2 accounts, got %d", len(items))
	}

	// Filter by provider.
	slackResp := authedGet(t, apiURL()+"/v1/connected-accounts?provider=slack")
	defer slackResp.Body.Close()
	if slackResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(slackResp.Body)
		t.Fatalf("filter provider: expected 200, got %d: %s", slackResp.StatusCode, body)
	}
	var slackResult map[string]any
	json.NewDecoder(slackResp.Body).Decode(&slackResult)
	slackItems, _ := slackResult["items"].([]any)
	for _, item := range slackItems {
		m, _ := item.(map[string]any)
		if stringField(m, "provider") != "slack" {
			t.Errorf("expected provider=slack, got %v", m["provider"])
		}
	}

	// Filter by external_user_id.
	extUserResp := authedGet(t, apiURL()+"/v1/connected-accounts?external_user_id=U_FILTER_TEST_1")
	defer extUserResp.Body.Close()
	if extUserResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(extUserResp.Body)
		t.Fatalf("filter external_user_id: expected 200, got %d: %s", extUserResp.StatusCode, body)
	}
	var extUserResult map[string]any
	json.NewDecoder(extUserResp.Body).Decode(&extUserResult)
	extUserItems, _ := extUserResult["items"].([]any)
	if len(extUserItems) != 1 {
		t.Fatalf("expected 1 result for external_user_id filter, got %d", len(extUserItems))
	}

	// Filter by external_team_id.
	extTeamResp := authedGet(t, apiURL()+"/v1/connected-accounts?external_team_id=T_FILTER_TEST")
	defer extTeamResp.Body.Close()
	if extTeamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(extTeamResp.Body)
		t.Fatalf("filter external_team_id: expected 200, got %d: %s", extTeamResp.StatusCode, body)
	}

	// Filter by status.
	activeResp := authedGet(t, apiURL()+"/v1/connected-accounts?status=active")
	defer activeResp.Body.Close()
	if activeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(activeResp.Body)
		t.Fatalf("filter status: expected 200, got %d: %s", activeResp.StatusCode, body)
	}

	// Clean up.
	authedDelete(t, apiURL()+"/v1/connected-accounts/"+id1).Body.Close()
	authedDelete(t, apiURL()+"/v1/connected-accounts/"+id2).Body.Close()
}

func TestConnectedAccounts_GetNotFound(t *testing.T) {
	resp := authedGet(t, apiURL()+"/v1/connected-accounts/conn_nonexistent")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestConnectedAccounts_DeleteNotFound(t *testing.T) {
	resp := authedDelete(t, apiURL()+"/v1/connected-accounts/conn_nonexistent")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestConnectedAccounts_CreateValidation(t *testing.T) {
	// Empty provider should fail.
	resp := authedPost(t, apiURL()+"/v1/connected-accounts", map[string]any{
		"provider": "",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty provider, got %d", resp.StatusCode)
	}
}

func TestConnectedAccounts_Unauthorized(t *testing.T) {
	resp, err := http.Get(apiURL() + "/v1/connected-accounts")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 401 or 200 (auth disabled), got %d", resp.StatusCode)
	}
}
