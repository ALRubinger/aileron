package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/config"
)

func TestLoadAileronConfig_MissingFileReturnsEmpty(t *testing.T) {
	cfg, err := config.LoadAileronConfig(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadAileronConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil empty config, got nil")
	}
	if cfg.Notifications != nil {
		t.Errorf("expected Notifications nil for missing file, got %+v", cfg.Notifications)
	}
}

func TestLoadAileronConfig_FullRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `notifications:
  slack:
    app_token: vault:slack-app
    bot_token: vault:slack-bot
    user_token: vault:slack-user
    channels:
      - name: "#engineering"
        show: mentions
        auto_draft: true
        priority: high
      - name: "#random"
        show: none
    ignore:
      - bot-noise
  discord:
    bot_token: vault:discord-bot
    channels:
      - name: "12345"
        auto_draft: false
  quiet_hours:
    start: "22:00"
    end: "06:00"
    timezone: America/New_York
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.LoadAileronConfig(path)
	if err != nil {
		t.Fatalf("LoadAileronConfig: %v", err)
	}
	if cfg.Notifications == nil {
		t.Fatal("Notifications should be set")
	}

	slack := cfg.Notifications.Slack
	if slack == nil {
		t.Fatal("Slack should be set")
	}
	if slack.AppToken != "vault:slack-app" || slack.BotToken != "vault:slack-bot" || slack.UserToken != "vault:slack-user" {
		t.Errorf("slack tokens: %+v", slack)
	}
	if len(slack.Channels) != 2 {
		t.Fatalf("got %d slack channels, want 2", len(slack.Channels))
	}
	if slack.Channels[0].Name != "#engineering" || slack.Channels[0].Show != "mentions" || !slack.Channels[0].AutoDraft || slack.Channels[0].Priority != "high" {
		t.Errorf("slack channel[0]: %+v", slack.Channels[0])
	}
	if len(slack.Ignore) != 1 || slack.Ignore[0] != "bot-noise" {
		t.Errorf("slack ignore: %+v", slack.Ignore)
	}

	discord := cfg.Notifications.Discord
	if discord == nil || discord.BotToken != "vault:discord-bot" {
		t.Errorf("discord: %+v", discord)
	}
	if len(discord.Channels) != 1 || discord.Channels[0].Name != "12345" {
		t.Errorf("discord channels: %+v", discord.Channels)
	}

	qh := cfg.Notifications.QuietHours
	if qh == nil {
		t.Fatal("QuietHours should be set")
	}
	if qh.Start != "22:00" || qh.End != "06:00" || qh.Timezone != "America/New_York" {
		t.Errorf("quiet hours: %+v", qh)
	}
}

func TestLoadAileronConfig_MalformedYAMLReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("notifications: [not a map"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := config.LoadAileronConfig(path)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error %v should mention parse", err)
	}
}

func TestLoadAileronConfig_EmptyFileIsOK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadAileronConfig(path)
	if err != nil {
		t.Fatalf("LoadAileronConfig: %v", err)
	}
	if cfg.Notifications != nil {
		t.Errorf("expected Notifications nil for empty file, got %+v", cfg.Notifications)
	}
}

func TestDefaultAileronConfigPath_UnderHome(t *testing.T) {
	t.Setenv("HOME", "/test/home")
	got := config.DefaultAileronConfigPath()
	want := filepath.Join("/test/home", ".aileron", "config.yaml")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
