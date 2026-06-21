package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth"
)

// localDaemonAuthMiddleware caveat-token contract (ADR-0024, #958):
//   - The master token retains full, unscoped /v1/* access.
//   - A caveat token carrying only one capability is rejected on routes
//     requiring a different capability.
//   - A caveat token bound to session A is rejected for session B's comms
//     path (defense in depth).
//   - A caveat token with the full sandbox cap set passes each of its
//     mapped routes.
//   - The health / handshake / gateway exemptions pass with no token at all.
//   - An unknown bearer (neither master nor a valid caveat) is 401.

// caveatTestSetup builds a middleware-wrapped handler whose terminal
// handler records that it ran (200), plus the issuer used to mint tokens.
func caveatTestSetup(t *testing.T, masterToken string) (http.Handler, *auth.CaveatIssuer, *bool) {
	t.Helper()
	key, err := auth.GenerateCaveatSigningKey()
	if err != nil {
		t.Fatalf("GenerateCaveatSigningKey: %v", err)
	}
	issuer := auth.NewCaveatIssuer(key, "aileron-daemon")

	reached := false
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := localDaemonAuthMiddleware(masterToken, newCaveatIssuer(key), terminal)
	return h, issuer, &reached
}

func doReq(t *testing.T, h http.Handler, method, path, bearer string) (int, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Len() > 0
}

func TestCaveat_MasterTokenFullAccess(t *testing.T) {
	h, _, reached := caveatTestSetup(t, "master")
	// A route the caveat vocabulary never covers (vault) must still be
	// reachable with the master token.
	status, _ := doReq(t, h, http.MethodPost, "/v1/vault/unlock", "master")
	if status != http.StatusOK {
		t.Fatalf("master token on /v1/vault/unlock = %d, want 200", status)
	}
	if !*reached {
		t.Fatal("master token did not reach terminal handler")
	}
}

func TestCaveat_OnlyRunCap_RejectedOnOtherRoutes(t *testing.T) {
	h, issuer, _ := caveatTestSetup(t, "master")
	tok, err := issuer.Issue("sess-A", []auth.Capability{auth.CapActionsRun})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// actions:run alone — the run route itself is permitted.
	if status, _ := doReq(t, h, http.MethodPost, "/v1/actions/send_email/run", tok); status != http.StatusOK {
		t.Errorf("run route with actions:run = %d, want 200", status)
	}
	// ...but the list, approvals-poll, and comms routes are forbidden.
	denied := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/actions"},
		{http.MethodGet, "/v1/actions/send_email"},
		{http.MethodGet, "/v1/action-approvals/abc/result"},
		{http.MethodGet, "/v1/sessions/sess-A/comms/messages"},
	}
	for _, d := range denied {
		status, _ := doReq(t, h, d.method, d.path, tok)
		if status != http.StatusForbidden {
			t.Errorf("%s %s with only actions:run = %d, want 403", d.method, d.path, status)
		}
	}
}

func TestCaveat_SessionBinding_RejectsOtherSession(t *testing.T) {
	h, issuer, _ := caveatTestSetup(t, "master")
	tok, err := issuer.Issue("sess-A", auth.SandboxMCPCaps)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Own session comms: permitted.
	if status, _ := doReq(t, h, http.MethodGet, "/v1/sessions/sess-A/comms/messages", tok); status != http.StatusOK {
		t.Errorf("own-session comms = %d, want 200", status)
	}
	// Another session's comms: forbidden even with the comms cap.
	if status, _ := doReq(t, h, http.MethodGet, "/v1/sessions/sess-B/comms/messages", tok); status != http.StatusForbidden {
		t.Errorf("cross-session comms = %d, want 403", status)
	}
}

func TestCaveat_FullCapSet_PassesMappedRoutes(t *testing.T) {
	h, issuer, _ := caveatTestSetup(t, "master")
	tok, err := issuer.Issue("sess-A", auth.SandboxMCPCaps)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ok := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/actions"},
		{http.MethodGet, "/v1/actions/send_email"},
		{http.MethodPost, "/v1/actions/send_email/run"},
		{http.MethodGet, "/v1/action-approvals/abc/result"},
		{http.MethodGet, "/v1/sessions/sess-A/comms/messages"},
		{http.MethodPost, "/v1/sessions/sess-A/comms/send"},
		{http.MethodPost, "/v1/sessions/sess-A/comms/draft"},
		{http.MethodPost, "/v1/sessions/sess-A/comms/http"},
	}
	for _, c := range ok {
		status, _ := doReq(t, h, c.method, c.path, tok)
		if status != http.StatusOK {
			t.Errorf("%s %s with full cap set = %d, want 200", c.method, c.path, status)
		}
	}
}

func TestCaveat_OutOfVocabularyRoute_Forbidden(t *testing.T) {
	h, issuer, _ := caveatTestSetup(t, "master")
	tok, err := issuer.Issue("sess-A", auth.SandboxMCPCaps)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Vault and other non-mapped routes must be 403 even with the full
	// caveat cap set: the caveat is scoped strictly below the master.
	for _, p := range []string{"/v1/vault/unlock", "/v1/bindings", "/v1/sessions/sess-A/end", "/v1/connectors/install"} {
		if status, _ := doReq(t, h, http.MethodPost, p, tok); status != http.StatusForbidden {
			t.Errorf("caveat on %s = %d, want 403", p, status)
		}
	}
}

func TestCaveat_ExemptionsPassWithoutToken(t *testing.T) {
	h, _, _ := caveatTestSetup(t, "master")
	for _, p := range []string{"/v1/health", "/v1/auth/handshake", "/v1/messages", "/v1/chat/completions"} {
		if status, _ := doReq(t, h, http.MethodGet, p, ""); status != http.StatusOK {
			t.Errorf("exempt path %s without token = %d, want 200 (reached handler)", p, status)
		}
	}
}

func TestCaveat_UnknownBearer_Unauthorized(t *testing.T) {
	h, _, _ := caveatTestSetup(t, "master")
	if status, _ := doReq(t, h, http.MethodGet, "/v1/actions", "not-a-real-token"); status != http.StatusUnauthorized {
		t.Errorf("garbage bearer = %d, want 401", status)
	}
	if status, _ := doReq(t, h, http.MethodGet, "/v1/actions", ""); status != http.StatusUnauthorized {
		t.Errorf("missing bearer = %d, want 401", status)
	}
}
