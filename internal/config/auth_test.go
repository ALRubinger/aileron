package config

import (
	"testing"
	"time"
)

func TestLoadAuthConfig_Disabled(t *testing.T) {
	// No DATABASE_URL means auth is disabled.
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("AILERON_JWT_SIGNING_KEY", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthEnabled() {
		t.Error("expected auth disabled when no DATABASE_URL")
	}
}

func TestLoadAuthConfig_Enabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AILERON_JWT_SIGNING_KEY", "test-key-for-signing")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AuthEnabled() {
		t.Error("expected auth enabled")
	}
	if cfg.DatabaseURL != "postgres://localhost/test" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

func TestLoadAuthConfig_MissingJWTKey(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AILERON_JWT_SIGNING_KEY", "")

	_, err := LoadAuthConfig()
	if err == nil {
		t.Fatal("expected error when JWT key missing with auth enabled")
	}
}

func TestLoadAuthConfig_Defaults(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("AILERON_JWT_SIGNING_KEY", "")
	t.Setenv("AILERON_JWT_ISSUER", "")
	t.Setenv("AILERON_ACCESS_TOKEN_TTL", "")
	t.Setenv("AILERON_REFRESH_TOKEN_TTL", "")
	t.Setenv("AILERON_UI_BASE_URL", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JWTIssuer != "aileron" {
		t.Errorf("JWTIssuer = %q, want aileron", cfg.JWTIssuer)
	}
	if cfg.AccessTokenTTL != 15*time.Minute {
		t.Errorf("AccessTokenTTL = %v, want 15m", cfg.AccessTokenTTL)
	}
	if cfg.RefreshTokenTTL != 7*24*time.Hour {
		t.Errorf("RefreshTokenTTL = %v, want 168h", cfg.RefreshTokenTTL)
	}
	if cfg.UIBaseURL != "/" {
		t.Errorf("UIBaseURL = %q, want /", cfg.UIBaseURL)
	}
}

func TestLoadAuthConfig_CustomDurations(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("AILERON_ACCESS_TOKEN_TTL", "30m")
	t.Setenv("AILERON_REFRESH_TOKEN_TTL", "48h")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AccessTokenTTL != 30*time.Minute {
		t.Errorf("AccessTokenTTL = %v, want 30m", cfg.AccessTokenTTL)
	}
	if cfg.RefreshTokenTTL != 48*time.Hour {
		t.Errorf("RefreshTokenTTL = %v, want 48h", cfg.RefreshTokenTTL)
	}
}

func TestLoadAuthConfig_InvalidDuration(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("AILERON_ACCESS_TOKEN_TTL", "not-a-duration")

	_, err := LoadAuthConfig()
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestLoadAuthConfig_GoogleSigninEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GOOGLE_SIGNIN_CLIENT_ID", "signin-id")
	t.Setenv("GOOGLE_SIGNIN_CLIENT_SECRET", "signin-secret")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GoogleSigninEnabled() {
		t.Error("expected Google sign-in enabled")
	}
	if cfg.GoogleSigninClientID != "signin-id" {
		t.Errorf("GoogleSigninClientID = %q", cfg.GoogleSigninClientID)
	}
}

func TestLoadAuthConfig_GoogleSigninDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GOOGLE_SIGNIN_CLIENT_ID", "")
	t.Setenv("GOOGLE_SIGNIN_CLIENT_SECRET", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GoogleSigninEnabled() {
		t.Error("expected Google sign-in disabled when credentials missing")
	}
}

func TestLoadAuthConfig_GoogleConnectorEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GOOGLE_CONNECTOR_CLIENT_ID", "connector-id")
	t.Setenv("GOOGLE_CONNECTOR_CLIENT_SECRET", "connector-secret")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GoogleConnectorEnabled() {
		t.Error("expected Google connector enabled")
	}
	if cfg.GoogleConnectorClientID != "connector-id" {
		t.Errorf("GoogleConnectorClientID = %q", cfg.GoogleConnectorClientID)
	}
}

func TestLoadAuthConfig_GoogleConnectorDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GOOGLE_CONNECTOR_CLIENT_ID", "")
	t.Setenv("GOOGLE_CONNECTOR_CLIENT_SECRET", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GoogleConnectorEnabled() {
		t.Error("expected Google connector disabled when credentials missing")
	}
}

func TestLoadAuthConfig_GitHubSigninEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GITHUB_SIGNIN_CLIENT_ID", "gh-signin-id")
	t.Setenv("GITHUB_SIGNIN_CLIENT_SECRET", "gh-signin-secret")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GitHubSigninEnabled() {
		t.Error("expected GitHub sign-in enabled")
	}
	if cfg.GitHubSigninClientID != "gh-signin-id" {
		t.Errorf("GitHubSigninClientID = %q", cfg.GitHubSigninClientID)
	}
}

func TestLoadAuthConfig_GitHubSigninDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GITHUB_SIGNIN_CLIENT_ID", "")
	t.Setenv("GITHUB_SIGNIN_CLIENT_SECRET", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitHubSigninEnabled() {
		t.Error("expected GitHub sign-in disabled when credentials missing")
	}
}

func TestLoadAuthConfig_GitHubConnectorEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GITHUB_CONNECTOR_CLIENT_ID", "gh-connector-id")
	t.Setenv("GITHUB_CONNECTOR_CLIENT_SECRET", "gh-connector-secret")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GitHubConnectorEnabled() {
		t.Error("expected GitHub connector enabled")
	}
	if cfg.GitHubConnectorClientID != "gh-connector-id" {
		t.Errorf("GitHubConnectorClientID = %q", cfg.GitHubConnectorClientID)
	}
}

func TestLoadAuthConfig_GitHubConnectorDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GITHUB_CONNECTOR_CLIENT_ID", "")
	t.Setenv("GITHUB_CONNECTOR_CLIENT_SECRET", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitHubConnectorEnabled() {
		t.Error("expected GitHub connector disabled when credentials missing")
	}
}

func TestLoadAuthConfig_ResendEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("RESEND_API_KEY", "re_test_key")
	t.Setenv("MAIL_FROM", "hello@example.com")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ResendEnabled() {
		t.Error("expected Resend enabled")
	}
	if cfg.ResendAPIKey != "re_test_key" {
		t.Errorf("ResendAPIKey = %q", cfg.ResendAPIKey)
	}
	if cfg.MailFrom != "hello@example.com" {
		t.Errorf("MailFrom = %q, want hello@example.com", cfg.MailFrom)
	}
}

func TestLoadAuthConfig_ResendDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("RESEND_API_KEY", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ResendEnabled() {
		t.Error("expected Resend disabled when API key missing")
	}
}

func TestKEKSessionTTL_Default(t *testing.T) {
	t.Setenv("AILERON_KEK_SESSION_TTL", "")
	cfg := &AuthConfig{}
	if got := cfg.KEKSessionTTL(); got != 24*time.Hour {
		t.Errorf("KEKSessionTTL = %v, want 24h", got)
	}
}

func TestKEKSessionTTL_Custom(t *testing.T) {
	t.Setenv("AILERON_KEK_SESSION_TTL", "1h")
	cfg := &AuthConfig{}
	if got := cfg.KEKSessionTTL(); got != time.Hour {
		t.Errorf("KEKSessionTTL = %v, want 1h", got)
	}
}

func TestKEKSessionTTL_Invalid(t *testing.T) {
	t.Setenv("AILERON_KEK_SESSION_TTL", "not-a-duration")
	cfg := &AuthConfig{}
	if got := cfg.KEKSessionTTL(); got != 24*time.Hour {
		t.Errorf("KEKSessionTTL = %v, want 24h (fallback)", got)
	}
}

func TestLoadAuthConfig_LLMEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-key")
	t.Setenv("AILERON_LLM_MODEL_SYNTHESIS", "claude-haiku-4-5")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.LLMEnabled() {
		t.Error("expected LLM enabled")
	}
	if cfg.AnthropicAPIKey != "sk-ant-test-key" {
		t.Errorf("AnthropicAPIKey = %q", cfg.AnthropicAPIKey)
	}
	if cfg.LLMModelSynthesis != "claude-haiku-4-5" {
		t.Errorf("LLMModelSynthesis = %q", cfg.LLMModelSynthesis)
	}
}

func TestLoadAuthConfig_LLMDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMEnabled() {
		t.Error("expected LLM disabled when API key missing")
	}
}

func TestLoadAuthConfig_LLMModelDefaults(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("AILERON_LLM_MODEL_RESEARCH", "")
	t.Setenv("AILERON_LLM_MODEL_SYNTHESIS", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMModelResearch != "claude-haiku-4-5-20251001" {
		t.Errorf("LLMModelResearch = %q, want claude-haiku-4-5-20251001", cfg.LLMModelResearch)
	}
	if cfg.LLMModelSynthesis != "claude-sonnet-4-6" {
		t.Errorf("LLMModelSynthesis = %q, want claude-sonnet-4-6", cfg.LLMModelSynthesis)
	}
}

func TestLoadAuthConfig_TrimsWhitespace(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", " sk-ant-key ")
	t.Setenv("GOOGLE_SIGNIN_CLIENT_ID", "google-id\t")
	t.Setenv("GITHUB_SIGNIN_CLIENT_ID", " github-id")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicAPIKey != "sk-ant-key" {
		t.Errorf("AnthropicAPIKey not trimmed: %q", cfg.AnthropicAPIKey)
	}
	if cfg.GoogleSigninClientID != "google-id" {
		t.Errorf("GoogleSigninClientID not trimmed: %q", cfg.GoogleSigninClientID)
	}
	if cfg.GitHubSigninClientID != "github-id" {
		t.Errorf("GitHubSigninClientID not trimmed: %q", cfg.GitHubSigninClientID)
	}
}

func TestLoadAuthConfig_TrimsWhitespace_EnvOrDefault(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("AILERON_LLM_MODEL_SYNTHESIS", " claude-haiku-4-5 ")
	t.Setenv("MAIL_FROM", " custom@example.com\n")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMModelSynthesis != "claude-haiku-4-5" {
		t.Errorf("LLMModelSynthesis not trimmed: %q", cfg.LLMModelSynthesis)
	}
	if cfg.MailFrom != "custom@example.com" {
		t.Errorf("MailFrom not trimmed: %q", cfg.MailFrom)
	}
}

func TestLoadAuthConfig_TrimsWhitespace_DatabaseURL(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", " postgres://localhost/test ")
	t.Setenv("AILERON_JWT_SIGNING_KEY", " test-key ")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabaseURL != "postgres://localhost/test" {
		t.Errorf("DatabaseURL not trimmed: %q", cfg.DatabaseURL)
	}
	if cfg.JWTSigningKey != "test-key" {
		t.Errorf("JWTSigningKey not trimmed: %q", cfg.JWTSigningKey)
	}
}


func TestLoadAuthConfig_MailFromDefault(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("MAIL_FROM", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MailFrom != "noreply@withaileron.ai" {
		t.Errorf("MailFrom = %q, want noreply@withaileron.ai", cfg.MailFrom)
	}
}
