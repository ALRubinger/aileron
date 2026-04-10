package launch_test

import (
	"testing"

	"github.com/ALRubinger/aileron/core/policy/launch"
)

func TestQuietHours_ParseFromYAML(t *testing.T) {
	pf, err := launch.Load(testdataPath("quiet_hours.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if pf.Notifications == nil {
		t.Fatal("expected Notifications to be non-nil")
	}
	qh := pf.Notifications.QuietHours
	if qh == nil {
		t.Fatal("expected QuietHours to be non-nil")
	}
	if qh.Start != "22:00" {
		t.Errorf("Start = %q, want '22:00'", qh.Start)
	}
	if qh.End != "08:00" {
		t.Errorf("End = %q, want '08:00'", qh.End)
	}
	if qh.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q, want 'America/New_York'", qh.Timezone)
	}
}

func TestQuietHours_ChannelPriority(t *testing.T) {
	pf, err := launch.Load(testdataPath("quiet_hours.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if pf.Notifications == nil || pf.Notifications.Slack == nil {
		t.Fatal("expected Slack notifications to be non-nil")
	}
	channels := pf.Notifications.Slack.Channels
	if len(channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(channels))
	}
	if channels[0].Priority != "high" {
		t.Errorf("channels[0].Priority = %q, want 'high'", channels[0].Priority)
	}
	if channels[1].Priority != "normal" {
		t.Errorf("channels[1].Priority = %q, want 'normal'", channels[1].Priority)
	}
}

func TestQuietHours_OmittedIsNil(t *testing.T) {
	pf, err := launch.Load(testdataPath("basic.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if pf.Notifications != nil {
		t.Error("expected Notifications to be nil for basic.yaml")
	}
}
