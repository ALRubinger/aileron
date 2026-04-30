package config

import (
	"testing"
)

// LoadGatewayConfig contract:
//   - Defaults to api.openai.com and api.anthropic.com when env is unset.
//   - Honors AILERON_OPENAI_BASE_URL and AILERON_ANTHROPIC_BASE_URL.
//   - Rejects URLs that don't parse, lack a scheme, or use a non-HTTP scheme.

func TestLoadGatewayConfig_Defaults(t *testing.T) {
	t.Setenv("AILERON_OPENAI_BASE_URL", "")
	t.Setenv("AILERON_ANTHROPIC_BASE_URL", "")

	cfg, err := LoadGatewayConfig()
	if err != nil {
		t.Fatalf("LoadGatewayConfig: %v", err)
	}
	if got, want := cfg.OpenAIBaseURL.String(), "https://api.openai.com"; got != want {
		t.Errorf("OpenAIBaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.AnthropicBaseURL.String(), "https://api.anthropic.com"; got != want {
		t.Errorf("AnthropicBaseURL = %q, want %q", got, want)
	}
}

func TestLoadGatewayConfig_EnvOverrides(t *testing.T) {
	t.Setenv("AILERON_OPENAI_BASE_URL", "http://localhost:9001")
	t.Setenv("AILERON_ANTHROPIC_BASE_URL", "http://localhost:9002/proxy")

	cfg, err := LoadGatewayConfig()
	if err != nil {
		t.Fatalf("LoadGatewayConfig: %v", err)
	}
	if got, want := cfg.OpenAIBaseURL.String(), "http://localhost:9001"; got != want {
		t.Errorf("OpenAIBaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.AnthropicBaseURL.String(), "http://localhost:9002/proxy"; got != want {
		t.Errorf("AnthropicBaseURL = %q, want %q", got, want)
	}
}

func TestLoadGatewayConfig_TrimsWhitespace(t *testing.T) {
	t.Setenv("AILERON_OPENAI_BASE_URL", "  https://upstream.example  ")

	cfg, err := LoadGatewayConfig()
	if err != nil {
		t.Fatalf("LoadGatewayConfig: %v", err)
	}
	if got, want := cfg.OpenAIBaseURL.String(), "https://upstream.example"; got != want {
		t.Errorf("OpenAIBaseURL = %q, want %q", got, want)
	}
}

func TestLoadGatewayConfig_RejectsInvalidScheme(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{"missing scheme", "AILERON_OPENAI_BASE_URL", "api.openai.com"},
		{"unsupported scheme", "AILERON_OPENAI_BASE_URL", "ftp://api.openai.com"},
		{"empty host", "AILERON_ANTHROPIC_BASE_URL", "https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			if _, err := LoadGatewayConfig(); err == nil {
				t.Errorf("expected error for %s=%q, got nil", tc.env, tc.val)
			}
		})
	}
}
