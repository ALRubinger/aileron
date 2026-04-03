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
	GoogleRedirectURL  string // Env: GOOGLE_REDIRECT_URL
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
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
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
