package launch

import (
	"bytes"
	"strings"
	"testing"
)

// printStartupBanner contract:
//
//   - No gateway: a single-line banner with session id + log path,
//     no URL.
//   - Gateway up + vault unlocked at startup: a single-line banner
//     with the webapp URL.
//   - Gateway up + vault locked (webapp prompt mode): adds a
//     follow-up line directing the user to the unlock surface (#429).

func TestPrintStartupBanner_NoGateway(t *testing.T) {
	buf := &bytes.Buffer{}
	printStartupBanner(buf, nil, "sid", "/tmp/log", false)
	out := buf.String()
	if !strings.Contains(out, "session sid") {
		t.Errorf("missing session id in banner: %q", out)
	}
	if strings.Contains(out, "webapp http") {
		t.Errorf("banner has a webapp URL but no gateway was supplied: %q", out)
	}
	if strings.Contains(out, "Vault locked") {
		t.Errorf("banner mentions vault locked but no gateway was supplied: %q", out)
	}
}

func TestPrintStartupBanner_GatewayUnlocked(t *testing.T) {
	buf := &bytes.Buffer{}
	g := &Gateway{URL: "http://127.0.0.1:54321"}
	printStartupBanner(buf, g, "sid", "/tmp/log", false)
	out := buf.String()
	if !strings.Contains(out, "webapp http://127.0.0.1:54321") {
		t.Errorf("banner missing webapp URL: %q", out)
	}
	if strings.Contains(out, "Vault locked") {
		t.Errorf("banner mentions vault locked when vaultLocked=false: %q", out)
	}
}

func TestPrintStartupBanner_GatewayLockedAddsUnlockHint(t *testing.T) {
	buf := &bytes.Buffer{}
	g := &Gateway{URL: "http://127.0.0.1:54321"}
	printStartupBanner(buf, g, "sid", "/tmp/log", true)
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
