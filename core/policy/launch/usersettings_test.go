package launch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/core/policy/launch"
)

func TestLoadUserSettings_NoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	pf, err := launch.LoadUserSettings()
	if err != nil {
		t.Fatalf("expected no error when settings file absent, got %v", err)
	}
	if pf.Version != 1 {
		t.Errorf("Version = %d, want 1", pf.Version)
	}
	if len(pf.Allow) != 0 {
		t.Errorf("expected no rules, got %d allow rules", len(pf.Allow))
	}
}

func TestLoadUserSettings_ValidFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)

	settings := `
version: 1
default: allow
allow:
  - "cat /tmp/*"
  - "ls /tmp/*"
ask:
  - command: "rm /tmp/*"
    description: "confirm before deleting temp files"
settings:
  timeout: 60
`
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(settings), 0o644)

	pf, err := launch.LoadUserSettings()
	if err != nil {
		t.Fatalf("LoadUserSettings failed: %v", err)
	}
	if len(pf.Allow) != 2 {
		t.Errorf("Allow = %d, want 2", len(pf.Allow))
	}
	if len(pf.Ask) != 1 {
		t.Errorf("Ask = %d, want 1", len(pf.Ask))
	}
	if pf.Default != "allow" {
		t.Errorf("Default = %q, want 'allow'", pf.Default)
	}
	if pf.Settings == nil || pf.Settings.Timeout != 60 {
		t.Errorf("expected Timeout=60, got %v", pf.Settings)
	}
}

func TestLoadUserSettings_NoHomeDir(t *testing.T) {
	// When HOME is empty/unset, UserHomeDir may fail on some systems.
	// LoadUserSettings should return an empty policy, not an error.
	t.Setenv("HOME", "")

	pf, err := launch.LoadUserSettings()
	if err != nil {
		t.Fatalf("expected no error when HOME is empty, got %v", err)
	}
	if pf.Version != 1 {
		t.Errorf("Version = %d, want 1", pf.Version)
	}
}

func TestLoadUserSettings_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte("{{invalid"), 0o644)

	_, err := launch.LoadUserSettings()
	if err == nil {
		t.Fatal("expected error for invalid YAML in user settings")
	}
}

func TestLoadWithProfiles_UserSettingsAsBase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write user settings with personal allow rules.
	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(`
version: 1
allow:
  - "cat /tmp/*"
settings:
  timeout: 120
`), 0o644)

	// Write a project policy.
	projectDir := filepath.Join(dir, "project")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, "aileron.yaml"), []byte(`
version: 1
default: ask
allow:
  - "go test ./..."
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
settings:
  timeout: 30
`), 0o644)

	pf, err := launch.LoadWithProfiles(filepath.Join(projectDir, "aileron.yaml"), nil)
	if err != nil {
		t.Fatalf("LoadWithProfiles failed: %v", err)
	}

	// User has 1 allow + project has 1 allow = 2.
	if len(pf.Allow) != 2 {
		t.Errorf("Allow = %d, want 2 (1 user + 1 project)", len(pf.Allow))
	}

	// Deny comes from project only.
	if len(pf.Deny) != 1 {
		t.Errorf("Deny = %d, want 1", len(pf.Deny))
	}

	// Project timeout (30) should override user timeout (120).
	if pf.Settings == nil || pf.Settings.Timeout != 30 {
		t.Errorf("Timeout = %v, want 30 (project wins)", pf.Settings)
	}
}

func TestLoadWithProfiles_ProjectOverridesUserDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(`
version: 1
default: allow
`), 0o644)

	projectDir := filepath.Join(dir, "project")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, "aileron.yaml"), []byte(`
version: 1
default: deny
`), 0o644)

	pf, err := launch.LoadWithProfiles(filepath.Join(projectDir, "aileron.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pf.Default != "deny" {
		t.Errorf("Default = %q, want 'deny' (project wins)", pf.Default)
	}
}

func TestParse_NotificationsConfig(t *testing.T) {
	yaml := `
version: 1
notifications:
  slack:
    app_token: xapp-test
    bot_token: xoxb-test
    channels:
      - name: "#backend"
        show: all
        auto_draft: true
      - name: "#incidents"
        show: all
        priority: high
    ignore:
      - "#random"
  discord:
    bot_token: discord-test
    channels:
      - name: "dev-chat"
        show: all
        auto_draft: true
`
	pf, err := launch.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if pf.Notifications == nil {
		t.Fatal("expected notifications config")
	}
	if pf.Notifications.Slack == nil {
		t.Fatal("expected slack config")
	}
	if pf.Notifications.Slack.AppToken != "xapp-test" {
		t.Errorf("AppToken = %q, want 'xapp-test'", pf.Notifications.Slack.AppToken)
	}
	if len(pf.Notifications.Slack.Channels) != 2 {
		t.Fatalf("Slack channels = %d, want 2", len(pf.Notifications.Slack.Channels))
	}
	ch := pf.Notifications.Slack.Channels[0]
	if ch.Name != "#backend" || ch.Show != "all" || !ch.AutoDraft {
		t.Errorf("channel 0 = %+v, want #backend/all/auto_draft", ch)
	}
	if len(pf.Notifications.Slack.Ignore) != 1 || pf.Notifications.Slack.Ignore[0] != "#random" {
		t.Errorf("Ignore = %v, want [#random]", pf.Notifications.Slack.Ignore)
	}
	if pf.Notifications.Discord == nil {
		t.Fatal("expected discord config")
	}
	if len(pf.Notifications.Discord.Channels) != 1 {
		t.Errorf("Discord channels = %d, want 1", len(pf.Notifications.Discord.Channels))
	}
}

func TestMerge_Notifications(t *testing.T) {
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				AppToken: "base-token",
				Channels: []launch.ChannelConfig{{Name: "#general", Show: "all"}},
			},
		},
	}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Discord: &launch.DiscordNotifyConfig{
				BotToken: "discord-token",
				Channels: []launch.ChannelConfig{{Name: "dev-chat", Show: "all"}},
			},
		},
	}
	merged := launch.Merge(base, overlay)

	if merged.Notifications == nil {
		t.Fatal("expected merged notifications")
	}
	// Slack from base should survive since overlay doesn't define it.
	if merged.Notifications.Slack == nil || merged.Notifications.Slack.AppToken != "base-token" {
		t.Error("expected Slack config from base to survive")
	}
	// Discord from overlay.
	if merged.Notifications.Discord == nil || merged.Notifications.Discord.BotToken != "discord-token" {
		t.Error("expected Discord config from overlay")
	}
}

func TestMerge_NotificationsBaseNil(t *testing.T) {
	base := &launch.PolicyFile{Version: 1}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{AppToken: "tok"},
		},
	}
	merged := launch.Merge(base, overlay)
	if merged.Notifications == nil || merged.Notifications.Slack == nil {
		t.Error("expected overlay notifications when base has none")
	}
}

func TestMerge_NotificationsOverlayNil(t *testing.T) {
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{AppToken: "tok"},
		},
	}
	overlay := &launch.PolicyFile{Version: 1}
	merged := launch.Merge(base, overlay)
	if merged.Notifications == nil || merged.Notifications.Slack == nil {
		t.Error("expected base notifications when overlay has none")
	}
}

func TestMerge_NotificationsOverlayWins(t *testing.T) {
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				AppToken: "base-token",
				Channels: []launch.ChannelConfig{{Name: "#general"}},
			},
		},
	}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				AppToken: "overlay-token",
				Channels: []launch.ChannelConfig{{Name: "#backend"}},
			},
		},
	}
	merged := launch.Merge(base, overlay)

	// Overlay's Slack replaces base's Slack entirely.
	if merged.Notifications.Slack.AppToken != "overlay-token" {
		t.Errorf("AppToken = %q, want 'overlay-token'", merged.Notifications.Slack.AppToken)
	}
	if len(merged.Notifications.Slack.Channels) != 1 || merged.Notifications.Slack.Channels[0].Name != "#backend" {
		t.Error("expected overlay's channels to replace base's")
	}
}

func TestLoadWithProfiles_NoUserSettings(t *testing.T) {
	// With no ~/.aileron/settings.yaml, LoadWithProfiles should still work.
	t.Setenv("HOME", t.TempDir())

	pf, err := launch.LoadWithProfiles(testdataPath("basic.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Allow) != 4 {
		t.Errorf("Allow = %d, want 4 (from basic.yaml only)", len(pf.Allow))
	}
}
