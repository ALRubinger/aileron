package agents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

// cannedTokenClient returns an *http.Client whose transport answers every
// request with the given status and body, with no network. It lets the
// claudeExchangeCode error branches be exercised against the hardcoded
// claudeTokenURL const without a real server.
type cannedRoundTripper struct {
	status int
	body   string
}

func (rt cannedRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: rt.status,
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Header:     make(http.Header),
	}, nil
}

func cannedTokenClient(status int, body string) *http.Client {
	return &http.Client{Transport: cannedRoundTripper{status: status, body: body}}
}

// TestClaudeExchangeCode_NonOKWithErrorCodeRedactsDescription asserts a
// token-error body surfaces only the HTTP status and the RFC 6749 `error`
// code; the free-text error_description (which may echo request material)
// must never reach the user-facing error.
func TestClaudeExchangeCode_NonOKWithErrorCodeRedactsDescription(t *testing.T) {
	const secret = "leaked-request-material-do-not-surface"
	client := cannedTokenClient(http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"`+secret+`"}`)

	_, _, _, _, err := claudeExchangeCode(context.Background(), client, "code", "ver", "state")
	if err == nil {
		t.Fatal("expected error on 400 token response, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "400") || !strings.Contains(msg, "invalid_grant") {
		t.Errorf("error %q should carry status 400 and the error code invalid_grant", msg)
	}
	if strings.Contains(msg, secret) {
		t.Errorf("error %q leaked the error_description; it must be redacted", msg)
	}
}

// TestClaudeExchangeCode_NonOKWithoutErrorCodeSurfacesBareStatus covers the
// branch where the error body does not parse as an RFC 6749 error object:
// only the bare HTTP status is surfaced.
func TestClaudeExchangeCode_NonOKWithoutErrorCodeSurfacesBareStatus(t *testing.T) {
	client := cannedTokenClient(http.StatusInternalServerError, "upstream proxy error, not json")

	_, _, _, _, err := claudeExchangeCode(context.Background(), client, "code", "ver", "state")
	if err == nil {
		t.Fatal("expected error on 500 token response, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("error %q should carry the bare status 500", msg)
	}
	if strings.Contains(msg, "not json") {
		t.Errorf("error %q leaked the raw body; it must be redacted", msg)
	}
}

// TestClaudeExchangeCode_MissingAccessToken covers a 2xx response whose
// JSON omits access_token: the exchange must fail rather than seed an
// unusable envelope.
func TestClaudeExchangeCode_MissingAccessToken(t *testing.T) {
	client := cannedTokenClient(http.StatusOK, `{"refresh_token":"r","expires_in":28800}`)

	_, _, _, _, err := claudeExchangeCode(context.Background(), client, "code", "ver", "state")
	if err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("expected missing-access_token error, got %v", err)
	}
}

// TestClaudeExchangeCode_MalformedJSON covers a 2xx response whose body is
// not valid JSON.
func TestClaudeExchangeCode_MalformedJSON(t *testing.T) {
	client := cannedTokenClient(http.StatusOK, `{not valid json`)

	_, _, _, _, err := claudeExchangeCode(context.Background(), client, "code", "ver", "state")
	if err == nil || !strings.Contains(err.Error(), "parse token response") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

// TestClaudeExchangeCode_OmittedScopeDefaultsToRequested covers the branch
// where the provider omits `scope` (RFC 6749 §5.1): the granted set
// defaults to the requested scopes, and expires_in is returned as seconds.
func TestClaudeExchangeCode_OmittedScopeDefaultsToRequested(t *testing.T) {
	client := cannedTokenClient(http.StatusOK,
		`{"access_token":"at","refresh_token":"rt","expires_in":28800,"token_type":"Bearer"}`)

	access, refresh, expiresIn, scopes, err := claudeExchangeCode(
		context.Background(), client, "code", "ver", "state")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if access != "at" || refresh != "rt" || expiresIn != 28800 {
		t.Errorf("got access=%q refresh=%q expiresIn=%d, want at/rt/28800", access, refresh, expiresIn)
	}
	if strings.Join(scopes, " ") != strings.Join(claudeOAuthScopes, " ") {
		t.Errorf("omitted scope should default to requested %v, got %v", claudeOAuthScopes, scopes)
	}
}

func TestParseClaudeTokenErrorCode(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"standard error", `{"error":"invalid_grant","error_description":"x"}`, "invalid_grant"},
		{"no error field", `{"foo":"bar"}`, ""},
		{"malformed body", `not json at all`, ""},
		{"empty body", ``, ""},
	}
	for _, tt := range tests {
		if got := parseClaudeTokenErrorCode([]byte(tt.body)); got != tt.want {
			t.Errorf("%s: parseClaudeTokenErrorCode(%q) = %q, want %q", tt.name, tt.body, got, tt.want)
		}
	}
}
