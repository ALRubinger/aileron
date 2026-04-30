package config

import (
	"fmt"
	"net/url"
)

// GatewayConfig holds upstream URLs for the dual-protocol LLM gateway.
// The agent's request is forwarded to the appropriate upstream based on
// which endpoint it hits — `/v1/chat/completions` → OpenAI base URL,
// `/v1/messages` → Anthropic base URL.
type GatewayConfig struct {
	// OpenAIBaseURL is the upstream root for /v1/chat/completions.
	// Env: AILERON_OPENAI_BASE_URL (default: "https://api.openai.com")
	OpenAIBaseURL *url.URL

	// AnthropicBaseURL is the upstream root for /v1/messages.
	// Env: AILERON_ANTHROPIC_BASE_URL (default: "https://api.anthropic.com")
	AnthropicBaseURL *url.URL
}

const (
	defaultOpenAIBaseURL    = "https://api.openai.com"
	defaultAnthropicBaseURL = "https://api.anthropic.com"
)

// LoadGatewayConfig reads gateway configuration from environment variables,
// applying defaults that point at the public OpenAI and Anthropic APIs.
// Returns an error only if a configured URL fails to parse.
func LoadGatewayConfig() (*GatewayConfig, error) {
	openAIRaw := envOrDefault("AILERON_OPENAI_BASE_URL", defaultOpenAIBaseURL)
	anthropicRaw := envOrDefault("AILERON_ANTHROPIC_BASE_URL", defaultAnthropicBaseURL)

	openAIURL, err := parseUpstreamURL(openAIRaw)
	if err != nil {
		return nil, fmt.Errorf("AILERON_OPENAI_BASE_URL: %w", err)
	}
	anthropicURL, err := parseUpstreamURL(anthropicRaw)
	if err != nil {
		return nil, fmt.Errorf("AILERON_ANTHROPIC_BASE_URL: %w", err)
	}

	return &GatewayConfig{
		OpenAIBaseURL:    openAIURL,
		AnthropicBaseURL: anthropicURL,
	}, nil
}

func parseUpstreamURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("host is required")
	}
	return u, nil
}
