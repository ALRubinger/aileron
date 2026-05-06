package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// AileronConfig is the user-level configuration the daemon reads at
// startup from `~/.aileron/config.yaml`. Per ADR-0012 (#454 step 9B),
// configuration that used to live in per-project `aileron.yaml` —
// notably the `notifications` section that drives Slack/Discord
// listeners — moves here. The daemon is user-scoped and long-lived;
// per-project notification config doesn't fit that ownership model.
//
// Missing or empty file is fine: the daemon runs with no listeners
// and the `read_messages` MCP tool returns empty.
type AileronConfig struct {
	Notifications *NotifyConfig `yaml:"notifications,omitempty"`
}

// NotifyConfig holds the Slack/Discord listener configuration plus
// the daemon-wide quiet-hours window. Type names mirror the
// pre-9B types in `internal/policy/launch/schema.go` so callers that
// migrated find a near-drop-in replacement under a different
// package.
type NotifyConfig struct {
	Slack      *SlackNotifyConfig   `yaml:"slack,omitempty"`
	Discord    *DiscordNotifyConfig `yaml:"discord,omitempty"`
	QuietHours *QuietHoursConfig    `yaml:"quiet_hours,omitempty"`
}

// SlackNotifyConfig configures Slack integration. Tokens are vault
// references (`vault:<name>`) — the daemon resolves them after the
// user unlocks the vault, then starts the listener.
type SlackNotifyConfig struct {
	AppToken  string          `yaml:"app_token,omitempty"`
	BotToken  string          `yaml:"bot_token,omitempty"`
	UserToken string          `yaml:"user_token,omitempty"`
	Channels  []ChannelConfig `yaml:"channels,omitempty"`
	Ignore    []string        `yaml:"ignore,omitempty"`
}

// DiscordNotifyConfig configures Discord integration.
type DiscordNotifyConfig struct {
	BotToken string          `yaml:"bot_token,omitempty"`
	Channels []ChannelConfig `yaml:"channels,omitempty"`
	Ignore   []string        `yaml:"ignore,omitempty"`
}

// ChannelConfig defines how a single channel is handled by an
// agent — what to surface, whether to auto-draft replies, and the
// priority shape for quiet-hours filtering.
type ChannelConfig struct {
	Name      string `yaml:"name"`
	Show      string `yaml:"show,omitempty"`       // "all", "mentions", "none"
	AutoDraft bool   `yaml:"auto_draft,omitempty"` // route to agent for draft reply
	Priority  string `yaml:"priority,omitempty"`   // "normal", "high"
}

// QuietHoursConfig defines a daily window during which non-high-priority
// notifications are suppressed. Messages still queue; the status-bar
// callback and any other onChange triggers don't fire.
type QuietHoursConfig struct {
	Start    string `yaml:"start"`              // "HH:MM" 24-hour, e.g. "22:00"
	End      string `yaml:"end"`                // "HH:MM" 24-hour, e.g. "08:00"
	Timezone string `yaml:"timezone,omitempty"` // IANA tz; defaults to local
}

// DefaultAileronConfigPath returns `~/.aileron/config.yaml`. Used by
// the daemon at startup and by `aileron status` for surfacing the
// merged configuration.
func DefaultAileronConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".aileron", "config.yaml")
	}
	return filepath.Join(home, ".aileron", "config.yaml")
}

// LoadAileronConfig reads `path` (typically [DefaultAileronConfigPath])
// and returns the parsed config. A missing file returns an empty
// config and no error — the daemon treats "no config" as "no
// listeners," which is the most common case for users who haven't
// wired Slack/Discord yet.
func LoadAileronConfig(path string) (*AileronConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &AileronConfig{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg AileronConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
