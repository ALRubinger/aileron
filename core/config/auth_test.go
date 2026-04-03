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
	t.Setenv("AILERON_UI_REDIRECT_URL", "")

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
	if cfg.UIRedirectURL != "/" {
		t.Errorf("UIRedirectURL = %q, want /", cfg.UIRedirectURL)
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

func TestLoadAuthConfig_GoogleEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GOOGLE_CLIENT_ID", "my-client-id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "my-secret")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GoogleEnabled() {
		t.Error("expected Google enabled")
	}
	if cfg.GoogleClientID != "my-client-id" {
		t.Errorf("GoogleClientID = %q", cfg.GoogleClientID)
	}
}

func TestLoadAuthConfig_GoogleDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GoogleEnabled() {
		t.Error("expected Google disabled when credentials missing")
	}
}

func TestLoadAuthConfig_GitHubEnabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "gh-client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "gh-secret")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GitHubEnabled() {
		t.Error("expected GitHub enabled")
	}
	if cfg.GitHubClientID != "gh-client-id" {
		t.Errorf("GitHubClientID = %q", cfg.GitHubClientID)
	}
}

func TestLoadAuthConfig_GitHubDisabled(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "")

	cfg, err := LoadAuthConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitHubEnabled() {
		t.Error("expected GitHub disabled when credentials missing")
	}
}
