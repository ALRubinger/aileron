package agents

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/oauth"
)

// Unit 1 contract: the pure OAuth builders.
//
//   - buildClaudeAuthorizeURL embeds the public client id, the S256
//     challenge + method, the hosted-callback redirect, the state, and
//     the requested scopes.
//   - claudeEnvelopeFromTokenResponse emits an envelope that passes
//     validateClaudeEnvelope and whose expiresAt is in MILLISECONDS.
//   - claudeEnvelopeFromBareToken populates accessToken only; refresh,
//     expiry, and scopes are absent.

func TestBuildClaudeAuthorizeURL_CarriesPKCEStateScopesAndCallback(t *testing.T) {
	pkce := oauth.PKCEPair{Verifier: "ver", Challenge: "chal-xyz", Method: "S256"}
	raw := buildClaudeAuthorizeURL(pkce, "state-abc")

	if !strings.HasPrefix(raw, claudeAuthorizeURL+"?") {
		t.Fatalf("authorize URL = %q, want prefix %q?", raw, claudeAuthorizeURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	checks := map[string]string{
		"client_id":             claudeOAuthClientID,
		"response_type":         "code",
		"redirect_uri":          claudeHostedCallbackURL,
		"state":                 "state-abc",
		"code_challenge":        "chal-xyz",
		"code_challenge_method": "S256",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("authorize URL %s = %q, want %q", k, got, want)
		}
	}
	if got := q.Get("scope"); got != strings.Join(claudeOAuthScopes, " ") {
		t.Errorf("authorize URL scope = %q, want %q", got, strings.Join(claudeOAuthScopes, " "))
	}
	// The hosted-callback paste mechanism is selected by code=true.
	if got := q.Get("code"); got != "true" {
		t.Errorf("authorize URL code = %q, want \"true\"", got)
	}
}

func TestClaudeEnvelopeFromTokenResponse_ExpiresAtIsMilliseconds(t *testing.T) {
	scopes := []string{"user:inference"}
	b, err := claudeEnvelopeFromTokenResponse("access-tok", "refresh-tok", 28800, scopes)
	if err != nil {
		t.Fatalf("claudeEnvelopeFromTokenResponse: %v", err)
	}
	if err := validateClaudeEnvelope(b); err != nil {
		t.Fatalf("built envelope failed validation: %v", err)
	}
	var env claudeCredentialEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.ClaudeAiOauth.AccessToken != "access-tok" {
		t.Errorf("accessToken = %q, want access-tok", env.ClaudeAiOauth.AccessToken)
	}
	if env.ClaudeAiOauth.RefreshToken != "refresh-tok" {
		t.Errorf("refreshToken = %q, want refresh-tok", env.ClaudeAiOauth.RefreshToken)
	}
	// expiresAt must be an absolute ms timestamp, not seconds and not a
	// relative duration. The real claude file carries ~1.7e12; anything
	// > 1e12 is unambiguously milliseconds-since-epoch (seconds-since-
	// epoch is ~1.7e9, three orders of magnitude smaller).
	if env.ClaudeAiOauth.ExpiresAt <= 1_000_000_000_000 {
		t.Errorf("expiresAt = %d, want milliseconds magnitude (> 1e12)", env.ClaudeAiOauth.ExpiresAt)
	}
	if len(env.ClaudeAiOauth.Scopes) != 1 || env.ClaudeAiOauth.Scopes[0] != "user:inference" {
		t.Errorf("scopes = %v, want [user:inference]", env.ClaudeAiOauth.Scopes)
	}
}

func TestClaudeEnvelopeFromTokenResponse_NoExpiryWhenZero(t *testing.T) {
	b, err := claudeEnvelopeFromTokenResponse("a", "r", 0, nil)
	if err != nil {
		t.Fatalf("claudeEnvelopeFromTokenResponse: %v", err)
	}
	var env claudeCredentialEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.ClaudeAiOauth.ExpiresAt != 0 {
		t.Errorf("expiresAt = %d, want 0 when expires_in is non-positive", env.ClaudeAiOauth.ExpiresAt)
	}
}

func TestClaudeEnvelopeFromBareToken_AccessTokenOnly(t *testing.T) {
	b, err := claudeEnvelopeFromBareToken("bare-long-lived-token")
	if err != nil {
		t.Fatalf("claudeEnvelopeFromBareToken: %v", err)
	}
	if err := validateClaudeEnvelope(b); err != nil {
		t.Fatalf("bare-token envelope failed validation: %v", err)
	}
	var env claudeCredentialEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.ClaudeAiOauth.AccessToken != "bare-long-lived-token" {
		t.Errorf("accessToken = %q, want bare-long-lived-token", env.ClaudeAiOauth.AccessToken)
	}
	if env.ClaudeAiOauth.RefreshToken != "" {
		t.Errorf("refreshToken = %q, want empty (setup-token has no refresh)", env.ClaudeAiOauth.RefreshToken)
	}
	if env.ClaudeAiOauth.ExpiresAt != 0 {
		t.Errorf("expiresAt = %d, want 0 (setup-token has no expiry)", env.ClaudeAiOauth.ExpiresAt)
	}
	if len(env.ClaudeAiOauth.Scopes) != 0 {
		t.Errorf("scopes = %v, want empty (setup-token has no scope list)", env.ClaudeAiOauth.Scopes)
	}
}

func TestSplitClaudeCodeState(t *testing.T) {
	tests := []struct {
		in, wantCode, wantState string
	}{
		{"abc#xyz", "abc", "xyz"},
		{"  abc#xyz  ", "abc", "xyz"},
		{"justcode", "justcode", ""},
		{"abc#xyz#extra", "abc", "xyz#extra"},
	}
	for _, tt := range tests {
		code, state := splitClaudeCodeState(tt.in)
		if code != tt.wantCode || state != tt.wantState {
			t.Errorf("splitClaudeCodeState(%q) = (%q,%q), want (%q,%q)",
				tt.in, code, state, tt.wantCode, tt.wantState)
		}
	}
}
