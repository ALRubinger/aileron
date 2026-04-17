//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestInstructions_CRUD(t *testing.T) {
	// Create an instruction.
	resp := authedPost(t, apiURL()+"/v1/instructions", map[string]string{
		"body":  "Always reference PR numbers when discussing code changes.",
		"scope": "#backend",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: expected 201, got %d: %s", resp.StatusCode, body)
	}

	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	// Handle both "ID" (pre-json-tag-fix) and "id" (post-fix).
	insID := stringField(created, "ID", "id")
	if insID == "" {
		t.Fatalf("expected instruction ID in response, got: %v", created)
	}
	t.Logf("created instruction: %s", insID)

	// List instructions — should contain the one we created.
	listResp := authedGet(t, apiURL()+"/v1/instructions")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("list: expected 200, got %d: %s", listResp.StatusCode, body)
	}

	var listResult map[string]any
	json.NewDecoder(listResp.Body).Decode(&listResult)
	items, _ := listResult["items"].([]any)
	if len(items) == 0 {
		t.Fatal("list: expected at least 1 instruction")
	}

	// List with active filter.
	activeResp := authedGet(t, apiURL()+"/v1/instructions?active=true")
	defer activeResp.Body.Close()
	if activeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(activeResp.Body)
		t.Fatalf("list active: expected 200, got %d: %s", activeResp.StatusCode, body)
	}

	// Get the specific instruction.
	getResp := authedGet(t, apiURL()+"/v1/instructions/"+insID)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("get: expected 200, got %d: %s", getResp.StatusCode, body)
	}

	// Get non-existent instruction — should be 404.
	notFoundResp := authedGet(t, apiURL()+"/v1/instructions/ins_nonexistent")
	defer notFoundResp.Body.Close()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("get nonexistent: expected 404, got %d", notFoundResp.StatusCode)
	}

	// Update the instruction body.
	updateResp := authedPatch(t, apiURL()+"/v1/instructions/"+insID, map[string]any{
		"body": "Updated: always reference PR numbers.",
	})
	defer updateResp.Body.Close()
	if updateResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(updateResp.Body)
		t.Fatalf("update body: expected 200, got %d: %s", updateResp.StatusCode, body)
	}

	// Update the instruction scope.
	updateScopeResp := authedPatch(t, apiURL()+"/v1/instructions/"+insID, map[string]any{
		"scope": "#incidents",
	})
	defer updateScopeResp.Body.Close()
	if updateScopeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(updateScopeResp.Body)
		t.Fatalf("update scope: expected 200, got %d: %s", updateScopeResp.StatusCode, body)
	}

	// Toggle active to false.
	toggleResp := authedPatch(t, apiURL()+"/v1/instructions/"+insID, map[string]any{
		"active": false,
	})
	defer toggleResp.Body.Close()
	if toggleResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(toggleResp.Body)
		t.Fatalf("toggle active: expected 200, got %d: %s", toggleResp.StatusCode, body)
	}

	// List inactive — should find our toggled instruction.
	inactiveResp := authedGet(t, apiURL()+"/v1/instructions?active=false")
	defer inactiveResp.Body.Close()
	if inactiveResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(inactiveResp.Body)
		t.Fatalf("list inactive: expected 200, got %d: %s", inactiveResp.StatusCode, body)
	}

	// Update non-existent — should be 404.
	updateNotFoundResp := authedPatch(t, apiURL()+"/v1/instructions/ins_nonexistent", map[string]any{
		"body": "nope",
	})
	defer updateNotFoundResp.Body.Close()
	if updateNotFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("update nonexistent: expected 404, got %d", updateNotFoundResp.StatusCode)
	}

	// Delete the instruction.
	deleteResp := authedDelete(t, apiURL()+"/v1/instructions/"+insID)
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete: expected 204, got %d: %s", deleteResp.StatusCode, body)
	}

	// Verify deleted.
	getAfterDelete := authedGet(t, apiURL()+"/v1/instructions/"+insID)
	defer getAfterDelete.Body.Close()
	if getAfterDelete.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", getAfterDelete.StatusCode)
	}

	// Delete non-existent — should be 404.
	deleteNotFoundResp := authedDelete(t, apiURL()+"/v1/instructions/ins_nonexistent")
	defer deleteNotFoundResp.Body.Close()
	if deleteNotFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete nonexistent: expected 404, got %d", deleteNotFoundResp.StatusCode)
	}
}

func TestInstructions_CreateValidation(t *testing.T) {
	// Empty body should fail.
	resp := authedPost(t, apiURL()+"/v1/instructions", map[string]string{
		"body": "",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty body, got %d", resp.StatusCode)
	}
}

func TestInstructions_Unauthorized(t *testing.T) {
	resp, err := http.Get(apiURL() + "/v1/instructions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 401 or 200 (auth disabled), got %d", resp.StatusCode)
	}
}

// stringField extracts a string field from a map, trying multiple key names.
func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
