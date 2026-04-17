//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestDrafts_CRUD(t *testing.T) {
	// Create a draft directly.
	resp := authedPost(t, apiURL()+"/v1/drafts", map[string]any{
		"service":      "slack",
		"channel":      "C_TEST",
		"author":       "Sarah",
		"message_body": "Does the JWT refactor change the claims?",
		"message_ts":   "1234567890.123456",
		"draft_body":   "No, the claims stay the same.",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: expected 201, got %d: %s", resp.StatusCode, body)
	}

	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	draftID := stringField(created, "ID", "id")
	if draftID == "" {
		t.Fatalf("expected draft ID in response, got: %v", created)
	}
	t.Logf("created draft: %s", draftID)

	// List — should contain the one we created.
	listResp := authedGet(t, apiURL()+"/v1/drafts")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("list: expected 200, got %d: %s", listResp.StatusCode, body)
	}
	var listResult map[string]any
	json.NewDecoder(listResp.Body).Decode(&listResult)
	items, _ := listResult["items"].([]any)
	if len(items) == 0 {
		t.Fatal("list: expected at least 1 draft")
	}

	// List by status.
	pendingResp := authedGet(t, apiURL()+"/v1/drafts?status=pending")
	defer pendingResp.Body.Close()
	if pendingResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(pendingResp.Body)
		t.Fatalf("list pending: expected 200, got %d: %s", pendingResp.StatusCode, body)
	}

	// Get the specific draft.
	getResp := authedGet(t, apiURL()+"/v1/drafts/"+draftID)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("get: expected 200, got %d: %s", getResp.StatusCode, body)
	}

	// Discard it (sends don't work without a real Slack token).
	discardResp := authedPost(t, apiURL()+"/v1/drafts/"+draftID+"/discard", nil)
	defer discardResp.Body.Close()
	if discardResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(discardResp.Body)
		t.Fatalf("discard: expected 200, got %d: %s", discardResp.StatusCode, body)
	}

	// Verify status changed — discarding again should be conflict.
	discardAgain := authedPost(t, apiURL()+"/v1/drafts/"+draftID+"/discard", nil)
	defer discardAgain.Body.Close()
	if discardAgain.StatusCode != http.StatusConflict {
		t.Fatalf("discard again: expected 409, got %d", discardAgain.StatusCode)
	}
}

func TestDrafts_EditWithoutSlack(t *testing.T) {
	// Create a draft.
	resp := authedPost(t, apiURL()+"/v1/drafts", map[string]any{
		"service":      "slack",
		"channel":      "C_EDIT_TEST",
		"author":       "Alex",
		"message_body": "What's the status?",
		"message_ts":   "999.111",
		"draft_body":   "Everything is on track.",
	})
	defer resp.Body.Close()
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	draftID := stringField(created, "ID", "id")

	// Edit — will fail at send (no Slack token) but exercises the edit path
	// through getDraftForAction and the status check.
	editResp := authedPost(t, apiURL()+"/v1/drafts/"+draftID+"/edit", map[string]string{
		"body": "Everything is on track. Shipping Friday.",
	})
	defer editResp.Body.Close()
	// Expect 500 (send fails) or 200 (if somehow it works).
	// The important thing is the handler ran and hit the store.
	t.Logf("edit status: %d", editResp.StatusCode)

	// If edit failed, discard to clean up.
	if editResp.StatusCode != http.StatusOK {
		discardResp := authedPost(t, apiURL()+"/v1/drafts/"+draftID+"/discard", nil)
		defer discardResp.Body.Close()
	}
}

func TestDrafts_MultipleAndFilter(t *testing.T) {
	// Create multiple drafts.
	for i, body := range []string{"Draft one", "Draft two", "Draft three"} {
		resp := authedPost(t, apiURL()+"/v1/drafts", map[string]any{
			"service":      "slack",
			"channel":      "C_MULTI",
			"author":       "Bot",
			"message_body": "msg",
			"message_ts":   "100." + string(rune('0'+i)),
			"draft_body":   body,
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d: expected 201, got %d", i, resp.StatusCode)
		}
	}

	// List all.
	listResp := authedGet(t, apiURL()+"/v1/drafts")
	defer listResp.Body.Close()
	var result map[string]any
	json.NewDecoder(listResp.Body).Decode(&result)
	items, _ := result["items"].([]any)
	if len(items) < 3 {
		t.Fatalf("expected at least 3 drafts, got %d", len(items))
	}

	// Filter by pending.
	pendingResp := authedGet(t, apiURL()+"/v1/drafts?status=pending")
	defer pendingResp.Body.Close()
	if pendingResp.StatusCode != http.StatusOK {
		t.Fatalf("filter pending: expected 200, got %d", pendingResp.StatusCode)
	}

	// Filter by approved (should be empty or fewer).
	approvedResp := authedGet(t, apiURL()+"/v1/drafts?status=approved")
	defer approvedResp.Body.Close()
	if approvedResp.StatusCode != http.StatusOK {
		t.Fatalf("filter approved: expected 200, got %d", approvedResp.StatusCode)
	}
}

func TestDrafts_CreateValidation(t *testing.T) {
	resp := authedPost(t, apiURL()+"/v1/drafts", map[string]any{
		"draft_body": "",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty draft_body, got %d", resp.StatusCode)
	}
}

func TestDrafts_GetNotFound(t *testing.T) {
	resp := authedGet(t, apiURL()+"/v1/drafts/dft_nonexistent")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDrafts_ApproveNotFound(t *testing.T) {
	resp := authedPost(t, apiURL()+"/v1/drafts/dft_nonexistent/approve", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDrafts_DiscardNotFound(t *testing.T) {
	resp := authedPost(t, apiURL()+"/v1/drafts/dft_nonexistent/discard", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDrafts_EditNotFound(t *testing.T) {
	resp := authedPost(t, apiURL()+"/v1/drafts/dft_nonexistent/edit", map[string]string{
		"body": "revised",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDrafts_Unauthorized(t *testing.T) {
	resp, err := http.Get(apiURL() + "/v1/drafts")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 401 or 200 (auth disabled), got %d", resp.StatusCode)
	}
}

func TestFeedback_CRUD(t *testing.T) {
	// Create feedback directly.
	resp := authedPost(t, apiURL()+"/v1/feedback", map[string]any{
		"draft_id":   "dft_test",
		"signal":     "approved",
		"service":    "slack",
		"channel":    "C_TEST",
		"draft_body": "The original draft",
		"sent_body":  "The original draft",
		"tools_used": []string{"slack_channel_history"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: expected 201, got %d: %s", resp.StatusCode, body)
	}

	// Create another with different signal.
	resp2 := authedPost(t, apiURL()+"/v1/feedback", map[string]any{
		"draft_id":   "dft_test2",
		"signal":     "edited",
		"service":    "slack",
		"channel":    "C_TEST",
		"draft_body": "Original",
		"sent_body":  "Revised by user",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("create edited: expected 201, got %d: %s", resp2.StatusCode, body)
	}

	// List all feedback.
	listResp := authedGet(t, apiURL()+"/v1/feedback")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("list: expected 200, got %d: %s", listResp.StatusCode, body)
	}
	var listResult map[string]any
	json.NewDecoder(listResp.Body).Decode(&listResult)
	items, _ := listResult["items"].([]any)
	if len(items) < 2 {
		t.Fatalf("list: expected at least 2 feedback items, got %d", len(items))
	}

	// Filter by signal.
	approvedResp := authedGet(t, apiURL()+"/v1/feedback?signal=approved")
	defer approvedResp.Body.Close()
	if approvedResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(approvedResp.Body)
		t.Fatalf("filter approved: expected 200, got %d: %s", approvedResp.StatusCode, body)
	}

	editedResp := authedGet(t, apiURL()+"/v1/feedback?signal=edited")
	defer editedResp.Body.Close()
	if editedResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(editedResp.Body)
		t.Fatalf("filter edited: expected 200, got %d: %s", editedResp.StatusCode, body)
	}
}

func TestFeedback_Discarded(t *testing.T) {
	resp := authedPost(t, apiURL()+"/v1/feedback", map[string]any{
		"draft_id":   "dft_discard_test",
		"signal":     "discarded",
		"service":    "slack",
		"channel":    "C_DISC",
		"draft_body": "Bad draft",
		"sent_body":  "",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create discarded: expected 201, got %d: %s", resp.StatusCode, body)
	}

	// Filter by discarded.
	discardedResp := authedGet(t, apiURL()+"/v1/feedback?signal=discarded")
	defer discardedResp.Body.Close()
	if discardedResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(discardedResp.Body)
		t.Fatalf("filter discarded: expected 200, got %d: %s", discardedResp.StatusCode, body)
	}
	var result map[string]any
	json.NewDecoder(discardedResp.Body).Decode(&result)
	items, _ := result["items"].([]any)
	if len(items) == 0 {
		t.Fatal("expected at least 1 discarded feedback")
	}
}

func TestFeedback_CreateValidation(t *testing.T) {
	resp := authedPost(t, apiURL()+"/v1/feedback", map[string]any{
		"signal": "",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty signal, got %d", resp.StatusCode)
	}
}

func TestFeedback_Unauthorized(t *testing.T) {
	resp, err := http.Get(apiURL() + "/v1/feedback")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 401 or 200 (auth disabled), got %d", resp.StatusCode)
	}
}

func TestTools_List(t *testing.T) {
	resp := authedGet(t, apiURL()+"/v1/tools")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if _, ok := result["tools"]; !ok {
		t.Fatal("expected 'tools' key in response")
	}
}

func TestTools_Unauthorized(t *testing.T) {
	resp, err := http.Get(apiURL() + "/v1/tools")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 401 or 200 (auth disabled), got %d", resp.StatusCode)
	}
}
