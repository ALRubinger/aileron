package agents_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Codex AuthSpec contract (U5):
//
//   - AuthSpec ships one FileBinding (auth.json) with a parent-dir
//     mount strategy so first-launch bootstrap can seed the vault,
//     plus a PreLaunchRefresh hook.
//   - Render/Capture are byte-identity over a valid chatgpt-mode
//     envelope.
//   - PreLaunchRefresh with a still-fresh access token is a no-op:
//     no HTTP call, no PUT, original secret returned.
//   - PreLaunchRefresh with an expired access token exchanges via
//     auth.openai.com, persists the rotated bundle via the daemon's
//     PutAgentCredentials, and returns the new secret.
//   - PreLaunchRefresh sanitizes provider responses: invalid_grant
//     errors mention the error code but NOT the raw body, so token
//     hints can't leak into user-facing errors per R21.
//   - The sandbox config.toml emission includes
//     `cli_auth_credentials_store = "file"` as a top-level key (R22).

func validCodexEnvelope() []byte {
	return []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","id_token":"id","account_id":"acct"},"last_refresh":"2026-06-01T00:00:00Z"}`)
}

func TestCodex_AuthSpec_Shape(t *testing.T) {
	spec := agents.Codex{}.AuthSpec()
	if len(spec.FileBindings) != 1 {
		t.Fatalf("FileBindings = %d, want 1", len(spec.FileBindings))
	}
	if len(spec.EnvBindings) != 0 || len(spec.StaticFiles) != 0 {
		t.Errorf("EnvBindings=%d StaticFiles=%d, want 0/0", len(spec.EnvBindings), len(spec.StaticFiles))
	}
	fb := spec.FileBindings[0]
	if fb.VaultPath != "agents/codex/oauth" {
		t.Errorf("VaultPath = %q, want agents/codex/oauth", fb.VaultPath)
	}
	if fb.ContainerPath != "/home/agent/.codex/auth.json" {
		t.Errorf("ContainerPath = %q, want /home/agent/.codex/auth.json", fb.ContainerPath)
	}
	if fb.Mode != 0o600 {
		t.Errorf("Mode = %v, want 0600", fb.Mode)
	}
	if fb.MountAsFile {
		t.Errorf("MountAsFile = true; first-launch bootstrap with an empty vault requires a parent-dir mount so the in-container login can write auth.json")
	}
	if fb.PreLaunchRefresh == nil {
		t.Errorf("PreLaunchRefresh is nil; Codex must declare a refresh hook")
	}
}

func TestCodex_Render_ByteIdentityOnValidEnvelope(t *testing.T) {
	envelope := validCodexEnvelope()
	spec := agents.Codex{}.AuthSpec()
	got, err := spec.FileBindings[0].Render(vault.Secret{Value: envelope})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(got, envelope) {
		t.Errorf("Render = %q, want byte-identity %q", got, envelope)
	}
}

func TestCodex_Render_RejectsEmptyVaultValue(t *testing.T) {
	spec := agents.Codex{}.AuthSpec()
	_, err := spec.FileBindings[0].Render(vault.Secret{Value: nil})
	if err == nil {
		t.Fatal("expected error for empty Value")
	}
}

func TestCodex_Render_RejectsNonChatGPTMode(t *testing.T) {
	spec := agents.Codex{}.AuthSpec()
	wrongMode := []byte(`{"auth_mode":"api_key","tokens":{"access_token":"a","refresh_token":"r"}}`)
	_, err := spec.FileBindings[0].Render(vault.Secret{Value: wrongMode})
	if err == nil {
		t.Fatal("expected error for non-chatgpt mode")
	}
}

func TestCodex_Capture_RoundTripStampsOAuthRefreshToken(t *testing.T) {
	envelope := validCodexEnvelope()
	spec := agents.Codex{}.AuthSpec()
	got, err := spec.FileBindings[0].Capture(envelope)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !bytes.Equal(got.Value, envelope) {
		t.Errorf("Capture Value = %q, want byte-identity %q", got.Value, envelope)
	}
	if got.Metadata.Type != "oauth_refresh_token" {
		t.Errorf("Metadata.Type = %q, want oauth_refresh_token", got.Metadata.Type)
	}
}

func TestCodex_Capture_RejectsMissingRefreshToken(t *testing.T) {
	// Schema drift: file parses as JSON but tokens.refresh_token is
	// absent. R13: launcher must skip the PUT rather than overwrite
	// vault with a usable-only-once envelope.
	envelope := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a"}}`)
	spec := agents.Codex{}.AuthSpec()
	_, err := spec.FileBindings[0].Capture(envelope)
	if err == nil {
		t.Fatal("expected error for envelope missing refresh_token")
	}
}

func TestCodex_PreLaunchRefresh_NoOpWhenStillFresh(t *testing.T) {
	// Future expiry → no refresh, no HTTP, no PUT.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	envelope := []byte(fmt.Sprintf(
		`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","expires_at":%q},"last_refresh":"2026-06-01T00:00:00Z"}`,
		future))
	spec := agents.Codex{}.AuthSpec()
	persisted := false
	got, err := spec.FileBindings[0].PreLaunchRefresh(
		vault.Secret{Value: envelope},
		launch.RefreshDeps{
			Ctx:        context.Background(),
			HTTPClient: refusingClient(t),
			PutAgentCredentials: func(s vault.Secret) error {
				persisted = true
				return nil
			},
		})
	if err != nil {
		t.Fatalf("PreLaunchRefresh: %v", err)
	}
	if persisted {
		t.Errorf("fresh token should not have triggered a vault PUT")
	}
	if !bytes.Equal(got.Value, envelope) {
		t.Errorf("fresh token should round-trip the input bytes")
	}
}

func TestCodex_PreLaunchRefresh_ExchangesExpiredToken(t *testing.T) {
	envelope := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"old","refresh_token":"r","expires_at":"2020-01-01T00:00:00Z"},"last_refresh":"2020-01-01T00:00:00Z"}`)

	gotForm := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		flat := map[string]string{}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				flat[k] = v[0]
			}
		}
		gotForm <- flat
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new",
			"refresh_token": "newrefresh",
			"id_token":      "newid",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()

	spec := agents.Codex{}.AuthSpec()
	persisted := make(chan vault.Secret, 1)
	got, err := spec.FileBindings[0].PreLaunchRefresh(
		vault.Secret{Value: envelope},
		launch.RefreshDeps{
			Ctx:        context.Background(),
			HTTPClient: redirectClient(srv.URL),
			PutAgentCredentials: func(s vault.Secret) error {
				persisted <- s
				return nil
			},
		})
	if err != nil {
		t.Fatalf("PreLaunchRefresh: %v", err)
	}

	form := <-gotForm
	if form["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", form["grant_type"])
	}
	if form["refresh_token"] != "r" {
		t.Errorf("refresh_token = %q, want r", form["refresh_token"])
	}

	select {
	case s := <-persisted:
		// The persisted envelope must carry the new access token and
		// the rotated refresh token. AE6 invariant: rotated bundle
		// is in vault BEFORE container start.
		if !strings.Contains(string(s.Value), `"access_token":"new"`) {
			t.Errorf("persisted envelope missing new access_token: %s", s.Value)
		}
		if !strings.Contains(string(s.Value), `"refresh_token":"newrefresh"`) {
			t.Errorf("persisted envelope missing newrefresh: %s", s.Value)
		}
		// The returned secret should equal the persisted secret —
		// the rotated bundle goes to vault THEN feeds Render.
		if !bytes.Equal(got.Value, s.Value) {
			t.Errorf("returned secret diverges from persisted secret")
		}
	default:
		t.Fatal("rotated bundle was not persisted via PutAgentCredentials")
	}
}

func TestCodex_PreLaunchRefresh_AbortsOnPersistFailure(t *testing.T) {
	envelope := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"old","refresh_token":"r","expires_at":"2020-01-01T00:00:00Z"}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new",
			"refresh_token": "newr",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	spec := agents.Codex{}.AuthSpec()
	_, err := spec.FileBindings[0].PreLaunchRefresh(
		vault.Secret{Value: envelope},
		launch.RefreshDeps{
			Ctx:        context.Background(),
			HTTPClient: redirectClient(srv.URL),
			PutAgentCredentials: func(s vault.Secret) error {
				return errors.New("daemon offline")
			},
		})
	if err == nil {
		t.Fatal("expected error when persist fails — silent-degrade is rejected per AE6")
	}
	if !strings.Contains(err.Error(), "vault") || !strings.Contains(err.Error(), "daemon offline") {
		t.Errorf("error = %v, want mention of vault persist failure", err)
	}
}

func TestCodex_PreLaunchRefresh_StripsProviderBodyFromError(t *testing.T) {
	// R21: raw provider response bodies must not appear in user-
	// facing errors (RFC 6749 §5.2 allows token hints in error
	// bodies). The error should mention the RFC 6749 standard
	// `error` field but NOT the human-readable
	// error_description that may include the token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token abc123def has been revoked"}`)
	}))
	defer srv.Close()

	envelope := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","expires_at":"2020-01-01T00:00:00Z"}}`)
	spec := agents.Codex{}.AuthSpec()
	_, err := spec.FileBindings[0].PreLaunchRefresh(
		vault.Secret{Value: envelope},
		launch.RefreshDeps{
			Ctx:        context.Background(),
			HTTPClient: redirectClient(srv.URL),
			PutAgentCredentials: func(s vault.Secret) error {
				t.Fatal("PUT should not be called when refresh fails")
				return nil
			},
		})
	if err == nil {
		t.Fatal("expected error on invalid_grant")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %v, want mention of standard error code invalid_grant", err)
	}
	if strings.Contains(err.Error(), "abc123def") {
		t.Errorf("error leaked raw provider body content: %v", err)
	}
	if !strings.Contains(err.Error(), "aileron vault delete") {
		t.Errorf("error = %v, want recovery hint", err)
	}
}

func TestCodex_PreLaunchRefresh_FreshnessDecisionAcrossExpiresAtShapes(t *testing.T) {
	// Three production shapes the refresh decision must handle:
	// future expiry (skip refresh), past expiry (refresh), empty
	// expires_at (refresh defensively because Codex CLI does not
	// populate the field today and a missing timestamp is
	// indistinguishable from "we don't know"), and malformed
	// expires_at (refresh defensively; rotated envelope overwrites
	// the bad value).
	tests := []struct {
		name        string
		expiresAt   string
		wantRefresh bool
	}{
		{"future-expiry skips refresh", time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), false},
		{"past-expiry triggers refresh", "2020-01-01T00:00:00Z", true},
		{"empty-expires-at triggers refresh", "", true},
		{"malformed-expires-at triggers refresh", "not-a-timestamp", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envelope := []byte(fmt.Sprintf(
				`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","expires_at":%q}}`,
				tc.expiresAt))
			refreshed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				refreshed = true
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "expires_in": 3600})
			}))
			defer srv.Close()

			spec := agents.Codex{}.AuthSpec()
			_, err := spec.FileBindings[0].PreLaunchRefresh(
				vault.Secret{Value: envelope},
				launch.RefreshDeps{
					Ctx:                 context.Background(),
					HTTPClient:          redirectClient(srv.URL),
					PutAgentCredentials: func(s vault.Secret) error { return nil },
				})
			if err != nil {
				t.Fatalf("PreLaunchRefresh: %v", err)
			}
			if refreshed != tc.wantRefresh {
				t.Errorf("refresh fired = %v, want %v", refreshed, tc.wantRefresh)
			}
		})
	}
}

func TestCodex_PreLaunchRefresh_MalformedEnvelopeFailsEarly(t *testing.T) {
	spec := agents.Codex{}.AuthSpec()
	_, err := spec.FileBindings[0].PreLaunchRefresh(
		vault.Secret{Value: []byte(`{"bogus":true}`)},
		launch.RefreshDeps{
			Ctx:        context.Background(),
			HTTPClient: refusingClient(t),
		})
	if err == nil {
		t.Fatal("expected error for malformed envelope")
	}
}

func TestCodex_SandboxConfig_EmitsCliAuthCredentialsStoreFile(t *testing.T) {
	// R22: sandbox-mode config.toml must include
	// `cli_auth_credentials_store = "file"` as a top-level key so
	// the in-container Codex resolves auth from auth.json instead
	// of trying a non-existent keyring.
	_, mounts, err := agents.Codex{}.ConfigureMCP("/usr/local/bin/aileron-mcp",
		map[string]string{"AILERON_URL": "http://x"}, "", launch.ModeSandbox)
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts = %d, want 1", len(mounts))
	}
	body, err := readFile(mounts[0].Source)
	if err != nil {
		t.Fatalf("read mount: %v", err)
	}
	if !strings.Contains(string(body), `cli_auth_credentials_store = "file"`) {
		t.Errorf("config.toml missing cli_auth_credentials_store top-level key:\n%s", body)
	}
	if !strings.Contains(string(body), "[mcp_servers.aileron]") {
		t.Errorf("config.toml missing [mcp_servers.aileron] block:\n%s", body)
	}
}

// --- helpers ---

// redirectClient returns an http.Client whose transport rewrites
// every outbound request to a single fixed host:port. Used to redirect
// the codex refresh hook's requests to a test server without changing
// the package-level codexTokenURL constant.
func redirectClient(targetURL string) *http.Client {
	return &http.Client{
		Transport: &redirectingTransport{target: targetURL},
		Timeout:   5 * time.Second,
	}
}

type redirectingTransport struct {
	target string
}

func (rt *redirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// Parse the target URL to swap scheme/host while preserving
	// the path the caller wanted (irrelevant here since the test
	// server accepts any path).
	u := req.URL
	tu, err := parseURL(rt.target)
	if err != nil {
		return nil, err
	}
	u.Scheme = tu.Scheme
	u.Host = tu.Host
	clone.URL = u
	clone.Host = tu.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// refusingClient returns an http.Client whose transport fails any
// request with a test fatal. Used to assert "no HTTP should happen
// on this code path."
func refusingClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{
		Transport: &refusingTransport{t: t},
	}
}

type refusingTransport struct {
	t *testing.T
}

func (rt *refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.t.Fatalf("unexpected HTTP request: %s %s", req.Method, req.URL.String())
	return nil, nil
}

func parseURL(s string) (*urlParts, error) {
	// Tiny parser that handles `scheme://host[:port]` without
	// importing net/url at the top (already imported below for
	// real use; this keeps the helper self-contained).
	const sep = "://"
	idx := strings.Index(s, sep)
	if idx < 0 {
		return nil, errors.New("redirectClient: target missing scheme")
	}
	scheme := s[:idx]
	rest := s[idx+len(sep):]
	if slash := strings.Index(rest, "/"); slash >= 0 {
		rest = rest[:slash]
	}
	return &urlParts{Scheme: scheme, Host: rest}, nil
}

type urlParts struct {
	Scheme, Host string
}

// readFile reads a host path; tiny indirection kept for clarity at
// the call site.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func TestCodex_Fresher(t *testing.T) {
	fresher := agents.Codex{}.AuthSpec().FileBindings[0].Fresher
	if fresher == nil {
		t.Fatal("Codex binding must supply a Fresher so a stale concurrent capture cannot clobber a fresher rotation")
	}
	// A valid chatgpt-mode envelope; expiresAt / lastRefresh optional.
	env := func(token, expiresAt, lastRefresh string) vault.Secret {
		tokens := `"access_token":"` + token + `","refresh_token":"r"`
		if expiresAt != "" {
			tokens += `,"expires_at":"` + expiresAt + `"`
		}
		body := `{"auth_mode":"chatgpt","tokens":{` + tokens + `}`
		if lastRefresh != "" {
			body += `,"last_refresh":"` + lastRefresh + `"`
		}
		body += `}`
		return vault.Secret{Value: []byte(body)}
	}
	t0 := "2026-06-01T00:00:00Z"
	t1 := "2026-06-02T00:00:00Z"
	tests := []struct {
		name              string
		captured, current vault.Secret
		want              bool
	}{
		{"newer expires_at is fresher", env("a", t1, ""), env("a", t0, ""), true},
		{"older expires_at is not fresher", env("a", t0, ""), env("a", t1, ""), false},
		{"equal expires_at is not fresher", env("a", t0, ""), env("a", t0, ""), false},
		{"last_refresh fallback when no expires_at", env("a", "", t1), env("a", "", t0), true},
		{"no timestamps, different token is a rotation, treat as fresher", env("new", "", ""), env("old", "", ""), true},
		{"no timestamps, same token is not fresher", env("a", "", ""), env("a", "", ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fresher(tt.captured, tt.current)
			if err != nil {
				t.Fatalf("fresher: %v", err)
			}
			if got != tt.want {
				t.Errorf("fresher = %v, want %v", got, tt.want)
			}
		})
	}

	// A malformed (non-chatgpt) envelope must error so the launcher
	// retains the prior entry.
	bad := vault.Secret{Value: []byte(`{"auth_mode":"api_key","tokens":{"access_token":"a"}}`)}
	if _, err := fresher(bad, env("a", t0, "")); err == nil {
		t.Error("expected error for malformed captured envelope")
	}
	if _, err := fresher(env("a", t0, ""), bad); err == nil {
		t.Error("expected error for malformed current envelope")
	}
}
