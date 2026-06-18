package launch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// daemon_client.go user-credentials contract (#1149):
//
//   - GetUserCredentials targets /v1/vault/user/{service}/credentials
//     (agent-independent namespace) and decodes a 200 envelope into
//     vault.Secret, including metadata round-trip.
//   - GET 404 with body code `vault_not_found` surfaces as
//     ErrUserCredentialsNotFound (distinct from a generic 404), so the
//     GitHub injector cleanly skips on an empty vault entry.
//   - GET 423 with body code `vault_locked` surfaces as
//     vault.ErrCredentialUnavailable (distinct from a 5xx).
//   - An unexpected 5xx must NOT collapse to ErrUserCredentialsNotFound.
//   - The bearer token is sent when configured.

func TestDaemonClient_GetUserCredentials_Success(t *testing.T) {
	want := vault.Secret{
		Value: []byte("ghp_exampletoken"),
		Metadata: vault.Metadata{
			Type:        "oauth_refresh_token",
			Environment: "personal",
			Labels:      map[string]string{"k": "v"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vault/user/github/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
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
	got, err := c.GetUserCredentials(context.Background(), "github")
	if err != nil {
		t.Fatalf("GetUserCredentials: %v", err)
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

func TestDaemonClient_GetUserCredentials_VaultNotFoundCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "vault_not_found",
				"message": "no credential entry for user/github",
			},
		})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.GetUserCredentials(context.Background(), "github")
	if !errors.Is(err, ErrUserCredentialsNotFound) {
		t.Fatalf("err = %v, want ErrUserCredentialsNotFound", err)
	}
}

func TestDaemonClient_GetUserCredentials_Bare404MapsToNotFound(t *testing.T) {
	// A bare 404 with no body code (an older daemon lacking the route,
	// or a generic not-found) must also map to ErrUserCredentialsNotFound
	// so the optional GitHub injection degrades to a clean,
	// unauthenticated launch instead of aborting.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.GetUserCredentials(context.Background(), "github")
	if !errors.Is(err, ErrUserCredentialsNotFound) {
		t.Fatalf("err = %v, want ErrUserCredentialsNotFound for a bare 404", err)
	}
}

func TestDaemonClient_GetUserCredentials_VaultLockedCode(t *testing.T) {
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
	_, err := c.GetUserCredentials(context.Background(), "github")
	if !errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Fatalf("err = %v, want vault.ErrCredentialUnavailable", err)
	}
}

func TestDaemonClient_GetUserCredentials_GenericNonOKReturnsStatusError(t *testing.T) {
	// An unexpected 5xx must NOT collapse to ErrUserCredentialsNotFound;
	// that would mistakenly treat a daemon outage as "no github token"
	// and silently skip injection. Status errors propagate as the
	// generic httpStatusError shape.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.GetUserCredentials(context.Background(), "github")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrUserCredentialsNotFound) {
		t.Errorf("err matched ErrUserCredentialsNotFound on 500; got %v", err)
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Errorf("err matched vault.ErrCredentialUnavailable on 500; got %v", err)
	}
}

func TestDaemonClient_GetUserCredentials_BearerTokenSent(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(agentCredentialsBody{Value: []byte("x")})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL, "tok_launch")
	if _, err := c.GetUserCredentials(context.Background(), "github"); err != nil {
		t.Fatalf("GetUserCredentials: %v", err)
	}
	if got := <-seen; !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("Authorization = %q, want bearer", got)
	}
}
