package launch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/policy/launch"
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

	pf, err := launch.LoadWithProfiles(filepath.Join(projectDir, "aileron.yaml"))
	if err != nil {
		t.Fatalf("LoadWithProfiles failed: %v", err)
	}

	// Should contain user allow + project allow + defaults.
	defaults := launch.DefaultPolicy()
	minAllow := len(defaults.Allow) + 2 // 1 user + 1 project
	if len(pf.Allow) < minAllow {
		t.Errorf("Allow = %d, want at least %d (defaults + user + project)", len(pf.Allow), minAllow)
	}

	// Deny includes defaults + project.
	minDeny := len(defaults.Deny) + 1
	if len(pf.Deny) < minDeny {
		t.Errorf("Deny = %d, want at least %d (defaults + project)", len(pf.Deny), minDeny)
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

	pf, err := launch.LoadWithProfiles(filepath.Join(projectDir, "aileron.yaml"))
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

	// Overlay token wins (last-writer-wins for tokens).
	if merged.Notifications.Slack.AppToken != "overlay-token" {
		t.Errorf("AppToken = %q, want 'overlay-token'", merged.Notifications.Slack.AppToken)
	}
	// Channels are unioned: base #general + overlay #backend = 2.
	if len(merged.Notifications.Slack.Channels) != 2 {
		t.Errorf("Channels = %d, want 2 (union of base and overlay)", len(merged.Notifications.Slack.Channels))
	}
}

func TestMerge_NotificationsPerChannelOverride(t *testing.T) {
	// Project sets #backend to show:all, auto_draft:true.
	// User overrides to show:mentions, auto_draft:false.
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				AppToken: "project-token",
				BotToken: "project-bot",
				Channels: []launch.ChannelConfig{
					{Name: "#backend", Show: "all", AutoDraft: true, Priority: "normal"},
				},
			},
		},
	}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				Channels: []launch.ChannelConfig{
					{Name: "#backend", Show: "mentions", AutoDraft: false},
				},
			},
		},
	}
	merged := launch.Merge(base, overlay)

	if merged.Notifications.Slack == nil {
		t.Fatal("expected merged slack config")
	}
	if len(merged.Notifications.Slack.Channels) != 1 {
		t.Fatalf("Channels = %d, want 1", len(merged.Notifications.Slack.Channels))
	}
	ch := merged.Notifications.Slack.Channels[0]
	if ch.Show != "mentions" {
		t.Errorf("Show = %q, want 'mentions' (user override)", ch.Show)
	}
	if ch.AutoDraft {
		t.Error("AutoDraft = true, want false (user override)")
	}
	// Priority not set in overlay, should keep base value.
	if ch.Priority != "normal" {
		t.Errorf("Priority = %q, want 'normal' (base preserved)", ch.Priority)
	}
	// Token should survive from base since overlay doesn't set it.
	if merged.Notifications.Slack.AppToken != "project-token" {
		t.Errorf("AppToken = %q, want 'project-token' (base preserved)", merged.Notifications.Slack.AppToken)
	}
}

func TestMerge_NotificationsChannelUnion(t *testing.T) {
	// Project has channels A, B; user adds channel C.
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				Channels: []launch.ChannelConfig{
					{Name: "#chanA", Show: "all"},
					{Name: "#chanB", Show: "mentions"},
				},
			},
		},
	}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				Channels: []launch.ChannelConfig{
					{Name: "#chanC", Show: "all", AutoDraft: true},
				},
			},
		},
	}
	merged := launch.Merge(base, overlay)

	channels := merged.Notifications.Slack.Channels
	if len(channels) != 3 {
		t.Fatalf("Channels = %d, want 3 (union of A, B, C)", len(channels))
	}
	names := make(map[string]bool)
	for _, ch := range channels {
		names[ch.Name] = true
	}
	for _, want := range []string{"#chanA", "#chanB", "#chanC"} {
		if !names[want] {
			t.Errorf("missing channel %s in merged result", want)
		}
	}
}

func TestMerge_NotificationsTokenOverride(t *testing.T) {
	// User can provide their own token (last-writer-wins).
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				AppToken: "project-app-token",
				BotToken: "project-bot-token",
			},
		},
	}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				BotToken: "user-bot-token",
			},
		},
	}
	merged := launch.Merge(base, overlay)

	// AppToken not set in overlay => base survives.
	if merged.Notifications.Slack.AppToken != "project-app-token" {
		t.Errorf("AppToken = %q, want 'project-app-token'", merged.Notifications.Slack.AppToken)
	}
	// BotToken set in overlay => overlay wins.
	if merged.Notifications.Slack.BotToken != "user-bot-token" {
		t.Errorf("BotToken = %q, want 'user-bot-token'", merged.Notifications.Slack.BotToken)
	}
}

func TestMerge_NotificationsIgnoreUnion(t *testing.T) {
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				Ignore: []string{"#random", "#spam"},
			},
		},
	}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Slack: &launch.SlackNotifyConfig{
				Ignore: []string{"#spam", "#offtopic"},
			},
		},
	}
	merged := launch.Merge(base, overlay)

	// Union with dedup: #random, #spam, #offtopic = 3.
	if len(merged.Notifications.Slack.Ignore) != 3 {
		t.Errorf("Ignore = %d, want 3 (union with dedup)", len(merged.Notifications.Slack.Ignore))
	}
}

func TestMerge_NotificationsDiscordPerChannel(t *testing.T) {
	base := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Discord: &launch.DiscordNotifyConfig{
				BotToken: "base-discord",
				Channels: []launch.ChannelConfig{
					{Name: "dev-chat", Show: "all", AutoDraft: true},
				},
				Ignore: []string{"memes"},
			},
		},
	}
	overlay := &launch.PolicyFile{
		Version: 1,
		Notifications: &launch.NotifyConfig{
			Discord: &launch.DiscordNotifyConfig{
				BotToken: "user-discord",
				Channels: []launch.ChannelConfig{
					{Name: "dev-chat", Show: "mentions"},
					{Name: "alerts", Show: "all", Priority: "high"},
				},
				Ignore: []string{"memes", "off-topic"},
			},
		},
	}
	merged := launch.Merge(base, overlay)

	dc := merged.Notifications.Discord
	if dc == nil {
		t.Fatal("expected discord config")
	}
	if dc.BotToken != "user-discord" {
		t.Errorf("BotToken = %q, want 'user-discord'", dc.BotToken)
	}
	if len(dc.Channels) != 2 {
		t.Fatalf("Channels = %d, want 2", len(dc.Channels))
	}
	// dev-chat should have overlay's Show, overlay's AutoDraft (false).
	for _, ch := range dc.Channels {
		if ch.Name == "dev-chat" {
			if ch.Show != "mentions" {
				t.Errorf("dev-chat Show = %q, want 'mentions'", ch.Show)
			}
			if ch.AutoDraft {
				t.Error("dev-chat AutoDraft = true, want false")
			}
		}
	}
	if len(dc.Ignore) != 2 {
		t.Errorf("Ignore = %d, want 2 (union with dedup)", len(dc.Ignore))
	}
}

func TestLoadWithProfiles_NoUserSettings(t *testing.T) {
	// With no ~/.aileron/settings.yaml, LoadWithProfiles should still work.
	t.Setenv("HOME", t.TempDir())

	pf, err := launch.LoadWithProfiles(testdataPath("basic.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Should have defaults + basic.yaml's 4 allow rules.
	defaults := launch.DefaultPolicy()
	minExpected := len(defaults.Allow) + 4
	if len(pf.Allow) < minExpected {
		t.Errorf("Allow = %d, want at least %d (defaults + basic.yaml)", len(pf.Allow), minExpected)
	}
}
