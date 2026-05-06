package launch

import (
	"bytes"
	"strings"
	"testing"
)

// printStartupBanner contract:
//
//   - No daemon URL: a single-line banner with session id + log path,
//     no URL.
//   - Daemon URL + vault unlocked: a single-line banner with the URL.
//   - Daemon URL + vault locked: adds a follow-up line directing the
//     user to the daemon URL to unlock (#429 modal lives there).

func TestPrintStartupBanner_NoDaemonURL(t *testing.T) {
	buf := &bytes.Buffer{}
	printStartupBanner(buf, "", "sid", "/tmp/log", false)
	out := buf.String()
	if !strings.Contains(out, "session sid") {
		t.Errorf("missing session id in banner: %q", out)
	}
	if strings.Contains(out, "webapp http") {
		t.Errorf("banner has a webapp URL but none was supplied: %q", out)
	}
	if strings.Contains(out, "Vault locked") {
		t.Errorf("banner mentions vault locked but no URL was supplied: %q", out)
	}
}

func TestPrintStartupBanner_UnlockedVault(t *testing.T) {
	buf := &bytes.Buffer{}
	printStartupBanner(buf, "http://127.0.0.1:54321", "sid", "/tmp/log", false)
	out := buf.String()
	if !strings.Contains(out, "webapp http://127.0.0.1:54321") {
		t.Errorf("banner missing webapp URL: %q", out)
	}
	if strings.Contains(out, "Vault locked") {
		t.Errorf("banner mentions vault locked when vaultLocked=false: %q", out)
	}
}

func TestPrintStartupBanner_LockedVaultAddsUnlockHint(t *testing.T) {
	buf := &bytes.Buffer{}
	printStartupBanner(buf, "http://127.0.0.1:54321", "sid", "/tmp/log", true)
	out := buf.String()
	if !strings.Contains(out, "webapp http://127.0.0.1:54321") {
		t.Errorf("banner missing webapp URL: %q", out)
	}
	if !strings.Contains(out, "Vault locked") {
		t.Errorf("banner missing unlock hint when vaultLocked=true: %q", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:54321") {
		t.Errorf("unlock hint missing the webapp URL: %q", out)
	}
}
