package capture

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultUserCapturePath_HonorsHome(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	got := DefaultUserCapturePath()
	want := filepath.Join("/home/tester", ".aileron", "capture-descriptors.yaml")
	if got != want {
		t.Errorf("DefaultUserCapturePath = %q, want %q", got, want)
	}
}

func TestDefaultUserCapturePath_FallsBackRelative(t *testing.T) {
	// An empty HOME makes os.UserHomeDir fail on unix, exercising the
	// relative fallback branch.
	t.Setenv("HOME", "")
	got := DefaultUserCapturePath()
	if !strings.HasSuffix(got, filepath.Join(".aileron", "capture-descriptors.yaml")) {
		t.Errorf("DefaultUserCapturePath = %q, want a .aileron-relative fallback", got)
	}
	if filepath.IsAbs(got) {
		t.Errorf("DefaultUserCapturePath = %q, want a relative fallback when HOME is unset", got)
	}
}

func TestDefaultCaptureLoadOptions_UsesDefaultUserPath(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	opts := DefaultCaptureLoadOptions()
	if opts.UserPath != DefaultUserCapturePath() {
		t.Errorf("UserPath = %q, want DefaultUserCapturePath", opts.UserPath)
	}
}
