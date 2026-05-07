package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// AileronConfig is the user-level configuration the daemon reads at
// startup from `~/.aileron/config.yaml`. Per ADR-0012 (#454 step 9B),
// configuration that used to live in per-project `aileron.yaml`
// moves here — the daemon is user-scoped and long-lived.
//
// Missing or empty file is fine.
type AileronConfig struct {
	Notifications *NotifyConfig `yaml:"notifications,omitempty"`
}

// NotifyConfig holds the daemon-wide quiet-hours window. Type names
// mirror the pre-9B types in `internal/policy/launch/schema.go` so
// callers that migrated find a near-drop-in replacement under a
// different package.
type NotifyConfig struct {
	QuietHours *QuietHoursConfig `yaml:"quiet_hours,omitempty"`
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
// config and no error.
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
