package config

import (
	"fmt"
	"os"
	"time"
)

// AuthConfig holds authentication configuration, loaded from environment
// variables. Secrets (client secrets, signing keys) must not be stored in
// the YAML config file.
type AuthConfig struct {
	// DatabaseURL is the PostgreSQL connection string.
	// Env: AILERON_DATABASE_URL
	DatabaseURL string

	// JWTSigningKey is the HMAC key for signing access tokens.
	// Env: AILERON_JWT_SIGNING_KEY
	JWTSigningKey string

	// JWTIssuer is the "iss" claim in issued tokens.
	// Env: AILERON_JWT_ISSUER (default: "aileron")
	JWTIssuer string

	// AccessTokenTTL is the lifetime of access tokens.
	// Env: AILERON_ACCESS_TOKEN_TTL (default: "15m")
	AccessTokenTTL time.Duration

	// RefreshTokenTTL is the lifetime of refresh tokens.
	// Env: AILERON_REFRESH_TOKEN_TTL (default: "168h" = 7 days)
	RefreshTokenTTL time.Duration

	// UIRedirectURL is where users are sent after successful auth.
	// Env: AILERON_UI_REDIRECT_URL (default: "/")
	UIRedirectURL string

	// AutoVerifyEmail skips email verification on signup, activating
	// accounts immediately. For development and CI only.
	// Env: AILERON_AUTO_VERIFY_EMAIL
	AutoVerifyEmail bool

	// Google OAuth configuration.
	GoogleClientID     string // Env: GOOGLE_CLIENT_ID
	GoogleClientSecret string // Env: GOOGLE_CLIENT_SECRET

	// GitHub OAuth configuration.
	GitHubClientID     string // Env: GITHUB_OAUTH_CLIENT_ID
	GitHubClientSecret string // Env: GITHUB_OAUTH_CLIENT_SECRET

	// Slack OAuth and Events API configuration.
	SlackClientID      string // Env: SLACK_CLIENT_ID
	SlackClientSecret  string // Env: SLACK_CLIENT_SECRET
	SlackSigningSecret string // Env: SLACK_SIGNING_SECRET (for webhook signature verification)

	// Resend email configuration.
	// ResendAPIKey enables real email sending via Resend. When set, ResendMailer
	// is used; otherwise LogMailer is used (prints to log, safe for dev/CI).
	// Env: RESEND_API_KEY
	ResendAPIKey string
	// MailFrom is the sender address for outgoing emails.
	// Env: MAIL_FROM (default: "noreply@withaileron.ai")
	MailFrom string

	// LLM configuration for cloud-hosted draft generation.
	AnthropicAPIKey string // Env: ANTHROPIC_API_KEY
	LLMModel        string // Env: AILERON_LLM_MODEL (default: "claude-sonnet-4-6")
}

// LoadAuthConfig loads auth configuration from environment variables.
// It returns an error only if required fields are missing when auth is
// enabled (indicated by AILERON_DATABASE_URL being set).
func LoadAuthConfig() (*AuthConfig, error) {
	cfg := &AuthConfig{
		DatabaseURL:        os.Getenv("AILERON_DATABASE_URL"),
		JWTSigningKey:      os.Getenv("AILERON_JWT_SIGNING_KEY"),
		JWTIssuer:          envOrDefault("AILERON_JWT_ISSUER", "aileron"),
		UIRedirectURL:      envOrDefault("AILERON_UI_REDIRECT_URL", "/"),
		AutoVerifyEmail:    os.Getenv("AILERON_AUTO_VERIFY_EMAIL") == "true",
		GoogleClientID:       os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:   os.Getenv("GOOGLE_CLIENT_SECRET"),
		GitHubClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		SlackClientID:      os.Getenv("SLACK_CLIENT_ID"),
		SlackClientSecret:  os.Getenv("SLACK_CLIENT_SECRET"),
		SlackSigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		ResendAPIKey:       os.Getenv("RESEND_API_KEY"),
		MailFrom:           envOrDefault("MAIL_FROM", "noreply@withaileron.ai"),
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		LLMModel:           envOrDefault("AILERON_LLM_MODEL", "claude-sonnet-4-6"),
	}

	// Parse durations with defaults.
	var err error
	cfg.AccessTokenTTL, err = parseDurationOrDefault("AILERON_ACCESS_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	cfg.RefreshTokenTTL, err = parseDurationOrDefault("AILERON_REFRESH_TOKEN_TTL", 7*24*time.Hour)
	if err != nil {
		return nil, err
	}

	// If no database URL is set, auth is disabled — return config as-is.
	if cfg.DatabaseURL == "" {
		return cfg, nil
	}

	// Validate required fields when auth is enabled.
	if cfg.JWTSigningKey == "" {
		return nil, fmt.Errorf("AILERON_JWT_SIGNING_KEY is required when AILERON_DATABASE_URL is set")
	}

	return cfg, nil
}

// AuthEnabled reports whether persistent auth is configured.
func (c *AuthConfig) AuthEnabled() bool {
	return c.DatabaseURL != ""
}

// GoogleEnabled reports whether Google OAuth is configured.
func (c *AuthConfig) GoogleEnabled() bool {
	return c.GoogleClientID != "" && c.GoogleClientSecret != ""
}

// GitHubEnabled reports whether GitHub OAuth is configured.
func (c *AuthConfig) GitHubEnabled() bool {
	return c.GitHubClientID != "" && c.GitHubClientSecret != ""
}

// SlackEnabled reports whether Slack OAuth and Events API are configured.
// Requires client credentials for the OAuth flow and a signing secret for
// webhook signature verification.
func (c *AuthConfig) SlackEnabled() bool {
	return c.SlackClientID != "" && c.SlackClientSecret != "" && c.SlackSigningSecret != ""
}

// LLMEnabled reports whether cloud-hosted draft generation is configured.
func (c *AuthConfig) LLMEnabled() bool {
	return c.AnthropicAPIKey != ""
}

// ResendEnabled reports whether Resend email delivery is configured.
func (c *AuthConfig) ResendEnabled() bool {
	return c.ResendAPIKey != ""
}

// KEKSessionTTL returns the TTL for in-memory KEK sessions.
// The KEK session controls how long the derived key stays in process memory
// for UI/management operations (viewing credentials, connecting accounts).
// Env: AILERON_KEK_SESSION_TTL (default: "24h")
func (c *AuthConfig) KEKSessionTTL() time.Duration {
	d, err := parseDurationOrDefault("AILERON_KEK_SESSION_TTL", 24*time.Hour)
	if err != nil {
		return 24 * time.Hour
	}
	return d
}

// EscrowTTL returns the TTL for credentials escrowed in TEE memory.
// Escrowed credentials persist independently of the KEK session, allowing
// autonomous agent execution without the user being online. The escrow is
// refreshed each time the user verifies their passphrase.
// Env: AILERON_ESCROW_TTL (default: "168h" = 7 days)
func (c *AuthConfig) EscrowTTL() time.Duration {
	d, err := parseDurationOrDefault("AILERON_ESCROW_TTL", 7*24*time.Hour)
	if err != nil {
		return 7 * 24 * time.Hour
	}
	return d
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDurationOrDefault(envKey string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(envKey)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", envKey, err)
	}
	return d, nil
}
