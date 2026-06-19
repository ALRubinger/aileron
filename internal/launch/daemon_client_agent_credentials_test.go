package launch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// daemon_client.go agent-credentials contract (U2):
//
//   - GetAgentCredentials decodes a 200 envelope into vault.Secret,
//     including metadata round-trip.
//   - GET 404 with body code `vault_not_found` surfaces as
//     ErrAgentCredentialsNotFound (distinct from a generic 404).
//   - GET 423 with body code `vault_locked` surfaces as
//     vault.ErrCredentialUnavailable (distinct from a 5xx).
//   - PutAgentCredentials sends a base64-encoded value on PUT and
//     succeeds on 204.
//   - PUT 423 surfaces as vault.ErrCredentialUnavailable.
//   - Both methods send the bearer token when configured.
//
// The contract test pins both error codes (`vault_not_found`,
// `vault_locked`) so that a future status-code change can't silently
// re-route the launcher's in-container-login fallthrough — the
// launcher discriminates by body code, not status alone.

func TestDaemonClient_GetAgentCredentials_Success(t *testing.T) {
	want := vault.Secret{
		Value: []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`),
		Metadata: vault.Metadata{
			Type:        "oauth_refresh_token",
			Environment: "personal",
			Labels:      map[string]string{"k": "v"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vault/agents/claude/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		// The default purpose (oauth) must not appear on the wire so a
		// default request is byte-for-byte identical to the pre-purpose
		// behavior.
		if r.URL.RawQuery != "" {
			t.Errorf("default GET sent query %q, want empty", r.URL.RawQuery)
		}
		body := agentCredentialsBody{
			Value: want.Value,
			Metadata: &agentCredentialsMetadataDTO{
				Type:        want.Metadata.Type,
				Environment: want.Metadata.Environment,
				Labels:      want.Metadata.Labels,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	got, err := c.GetAgentCredentials(context.Background(), "claude", "oauth")
	if err != nil {
		t.Fatalf("GetAgentCredentials: %v", err)
	}
	if string(got.Value) != string(want.Value) {
		t.Errorf("Value = %q, want %q", got.Value, want.Value)
	}
	if got.Metadata.Type != "oauth_refresh_token" {
		t.Errorf("Metadata.Type = %q, want oauth_refresh_token", got.Metadata.Type)
	}
	if got.Metadata.Environment != "personal" {
		t.Errorf("Metadata.Environment = %q, want personal", got.Metadata.Environment)
	}
	if got.Metadata.Labels["k"] != "v" {
		t.Errorf("Metadata.Labels[k] = %q, want v", got.Metadata.Labels["k"])
	}
}

func TestDaemonClient_GetAgentCredentials_VaultNotFoundCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "vault_not_found",
				"message": "no credential entry for agent claude",
			},
		})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.GetAgentCredentials(context.Background(), "claude", "oauth")
	if !errors.Is(err, ErrAgentCredentialsNotFound) {
		t.Fatalf("err = %v, want ErrAgentCredentialsNotFound", err)
	}
}

func TestDaemonClient_GetAgentCredentials_VaultLockedCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "vault_locked",
				"message": "unlock the vault first",
			},
		})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.GetAgentCredentials(context.Background(), "claude", "oauth")
	if !errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Fatalf("err = %v, want vault.ErrCredentialUnavailable", err)
	}
}

func TestDaemonClient_GetAgentCredentials_GenericNonOKReturnsStatusError(t *testing.T) {
	// Unexpected 5xx must NOT collapse to ErrAgentCredentialsNotFound
	// — that would mistakenly trigger the in-container-login bootstrap
	// on a daemon outage. Status errors propagate as the generic
	// httpStatusError shape so callers see the real failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.GetAgentCredentials(context.Background(), "claude", "oauth")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrAgentCredentialsNotFound) {
		t.Errorf("err matched ErrAgentCredentialsNotFound on 500; got %v", err)
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Errorf("err matched vault.ErrCredentialUnavailable on 500; got %v", err)
	}
}

func TestDaemonClient_PutAgentCredentials_SuccessAndPayload(t *testing.T) {
	gotBody := make(chan agentCredentialsBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/vault/agents/codex/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body agentCredentialsBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotBody <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	secret := vault.Secret{
		Value: []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r"}}`),
		Metadata: vault.Metadata{
			Type: "oauth_refresh_token",
		},
	}
	if err := c.PutAgentCredentials(context.Background(), "codex", "oauth", secret); err != nil {
		t.Fatalf("PutAgentCredentials: %v", err)
	}
	body := <-gotBody
	if string(body.Value) != string(secret.Value) {
		// encoding/json on []byte is base64; decode and compare.
		decoded, err := base64.StdEncoding.DecodeString(string(body.Value))
		if err != nil || string(decoded) != string(secret.Value) {
			t.Errorf("body.Value round-trip mismatch: got %q want %q (decoded=%q, err=%v)",
				body.Value, secret.Value, decoded, err)
		}
	}
	if body.Metadata == nil || body.Metadata.Type != "oauth_refresh_token" {
		t.Errorf("body.Metadata = %+v, want Type=oauth_refresh_token", body.Metadata)
	}
}

func TestDaemonClient_PutAgentCredentials_VaultLockedCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "vault_locked",
				"message": "unlock the vault first",
			},
		})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	err := c.PutAgentCredentials(context.Background(), "claude", "oauth", vault.Secret{Value: []byte("x")})
	if !errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Fatalf("err = %v, want vault.ErrCredentialUnavailable", err)
	}
}

func TestDaemonClient_AgentCredentials_BearerTokenSent(t *testing.T) {
	seen := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(agentCredentialsBody{Value: []byte("x")})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL, "tok_launch")
	if _, err := c.GetAgentCredentials(context.Background(), "claude", "oauth"); err != nil {
		t.Fatalf("GetAgentCredentials: %v", err)
	}
	if err := c.PutAgentCredentials(context.Background(), "claude", "oauth", vault.Secret{Value: []byte("x")}); err != nil {
		t.Fatalf("PutAgentCredentials: %v", err)
	}
	for i := 0; i < 2; i++ {
		if got := <-seen; !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want bearer", got)
		}
	}
}

// TestDaemonClient_GetAgentCredentials_NonDefaultPurpose verifies a
// non-oauth purpose is carried as the `?purpose=<value>` query so the
// daemon routes to `agents/<name>/<purpose>`. This is the SUB2/SUB3
// foundation: a second credential purpose for the same agent.
func TestDaemonClient_GetAgentCredentials_NonDefaultPurpose(t *testing.T) {
	want := []byte(`{"apiKey":"sk-test"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vault/agents/claude/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("purpose"); got != "apikey" {
			t.Errorf("purpose query = %q, want apikey", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentCredentialsBody{Value: want})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	got, err := c.GetAgentCredentials(context.Background(), "claude", "apikey")
	if err != nil {
		t.Fatalf("GetAgentCredentials: %v", err)
	}
	if string(got.Value) != string(want) {
		t.Errorf("Value = %q, want %q", got.Value, want)
	}
}

// TestDaemonClient_PutAgentCredentials_NonDefaultPurpose verifies a PUT
// for a non-oauth purpose hits `?purpose=<value>` and round-trips the
// written bytes.
func TestDaemonClient_PutAgentCredentials_NonDefaultPurpose(t *testing.T) {
	var gotPath, gotPurpose string
	var gotBody agentCredentialsBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPurpose = r.URL.Query().Get("purpose")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	secret := vault.Secret{Value: []byte(`{"apiKey":"sk-test"}`)}
	if err := c.PutAgentCredentials(context.Background(), "claude", "apikey", secret); err != nil {
		t.Fatalf("PutAgentCredentials: %v", err)
	}
	if gotPath != "/v1/vault/agents/claude/credentials" {
		t.Errorf("path = %q, want /v1/vault/agents/claude/credentials", gotPath)
	}
	if gotPurpose != "apikey" {
		t.Errorf("purpose query = %q, want apikey", gotPurpose)
	}
	if string(gotBody.Value) != string(secret.Value) {
		t.Errorf("PUT value = %q, want %q", gotBody.Value, secret.Value)
	}
}

// TestDaemonClient_PutAgentCredentials_DefaultPurposeWireUnchanged
// asserts the default purpose sends no query string on a PUT, keeping
// the wire byte-for-byte identical to the pre-purpose behavior.
func TestDaemonClient_PutAgentCredentials_DefaultPurposeWireUnchanged(t *testing.T) {
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	if err := c.PutAgentCredentials(context.Background(), "claude", "oauth", vault.Secret{Value: []byte("x")}); err != nil {
		t.Fatalf("PutAgentCredentials: %v", err)
	}
	if gotRawQuery != "" {
		t.Errorf("default PUT sent query %q, want empty", gotRawQuery)
	}
}
