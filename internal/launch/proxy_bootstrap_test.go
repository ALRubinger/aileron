package launch

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSandboxProxyBootstrap_GeneratesSessionCAAndMount(t *testing.T) {
	stateDir := t.TempDir()
	got, err := prepareSandboxProxyBootstrap(stateDir, "session-123", "http://host.docker.internal:48123/", "daemon-token")
	if err != nil {
		t.Fatalf("prepareSandboxProxyBootstrap: %v", err)
	}
	if got.Mode != "bootstrap" {
		t.Fatalf("Mode = %q, want bootstrap", got.Mode)
	}
	if got.ProxyURL != "http://session-123:daemon-token@host.docker.internal:48123" {
		t.Fatalf("ProxyURL = %q", got.ProxyURL)
	}
	wantCAPath := filepath.Join(stateDir, "sessions", "session-123", "sandbox-proxy", "ca.pem")
	if got.CAPath != wantCAPath {
		t.Fatalf("CAPath = %q, want %q", got.CAPath, wantCAPath)
	}
	if len(got.Mounts) != 1 {
		t.Fatalf("mounts = %+v, want one", got.Mounts)
	}
	if got.Mounts[0].Source != got.CAPath || got.Mounts[0].Target != sandboxProxyCAPath || !got.Mounts[0].ReadOnly {
		t.Fatalf("mount = %+v", got.Mounts[0])
	}

	data, err := os.ReadFile(got.CAPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CA PEM block = %#v", block)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	if !cert.IsCA {
		t.Fatal("generated certificate is not a CA")
	}
	if _, err := os.Stat(got.KeyPath); err != nil {
		t.Fatalf("stat CA key: %v", err)
	}
}

func TestPrepareSandboxProxyBootstrap_RejectsUnsafeSessionID(t *testing.T) {
	for _, sessionID := range []string{"../escape", "nested/session", `nested\session`, "session:123", " session-123", "session-123 ", ".", ".."} {
		t.Run(sessionID, func(t *testing.T) {
			_, err := prepareSandboxProxyBootstrap(t.TempDir(), sessionID, "http://host.docker.internal:48123", "")
			if err == nil {
				t.Fatal("expected unsafe session id error")
			}
		})
	}
}

func TestPrepareSandboxProxyBootstrap_RequiresAgentEndpointURL(t *testing.T) {
	_, err := prepareSandboxProxyBootstrap(t.TempDir(), "session-123", "", "")
	if err == nil {
		t.Fatal("expected agent endpoint URL error")
	}
}

func TestPrepareSandboxProxyBootstrap_RejectsInvalidAgentEndpointURL(t *testing.T) {
	_, err := prepareSandboxProxyBootstrap(t.TempDir(), "session-123", "http://", "")
	if err == nil {
		t.Fatal("expected invalid agent endpoint URL error")
	}
}

func TestPrepareToolContainerProxy_WritesCARewritesURLAndMountsRO(t *testing.T) {
	stateDir := t.TempDir()
	// A loopback daemon root (no /v1) must be rewritten to host.docker.internal
	// and carry the sessionID:token userinfo the CONNECT handshake needs.
	got, err := PrepareToolContainerProxy(stateDir, "session-abc", "http://127.0.0.1:48123", "docker", "daemon-token")
	if err != nil {
		t.Fatalf("PrepareToolContainerProxy: %v", err)
	}
	if got.ProxyURL != "http://session-abc:daemon-token@host.docker.internal:48123" {
		t.Fatalf("ProxyURL = %q, want loopback rewritten + authed", got.ProxyURL)
	}
	if got.CAContainerPath != sandboxProxyCAPath {
		t.Fatalf("CAContainerPath = %q, want %q", got.CAContainerPath, sandboxProxyCAPath)
	}
	wantSource := filepath.Join(stateDir, "sessions", "session-abc", "sandbox-proxy", "ca.pem")
	if got.CAMount.Source != wantSource {
		t.Fatalf("CAMount.Source = %q, want %q", got.CAMount.Source, wantSource)
	}
	if got.CAMount.Target != sandboxProxyCAPath || !got.CAMount.ReadOnly {
		t.Fatalf("CAMount = %+v, want RO at %s", got.CAMount, sandboxProxyCAPath)
	}
	// The CA on disk is a real CA certificate the daemon reader can load, and the
	// signing key is written alongside it.
	data, err := os.ReadFile(got.CAMount.Source)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CA PEM block = %#v", block)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	if !cert.IsCA {
		t.Fatal("generated certificate is not a CA")
	}
	keyPath := filepath.Join(stateDir, "sessions", "session-abc", "sandbox-proxy", "ca.key")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stat CA key: %v", err)
	}
}

func TestPrepareToolContainerProxy_NoTokenOmitsPassword(t *testing.T) {
	got, err := PrepareToolContainerProxy(t.TempDir(), "session-abc", "http://localhost:9999", "docker", "")
	if err != nil {
		t.Fatalf("PrepareToolContainerProxy: %v", err)
	}
	if got.ProxyURL != "http://session-abc@host.docker.internal:9999" {
		t.Fatalf("ProxyURL = %q, want session userinfo with no password", got.ProxyURL)
	}
}

func TestPrepareToolContainerProxy_RejectsBadInput(t *testing.T) {
	if _, err := PrepareToolContainerProxy(t.TempDir(), "", "http://127.0.0.1:1", "docker", ""); err == nil {
		t.Fatal("expected empty session id error")
	}
	if _, err := PrepareToolContainerProxy(t.TempDir(), "../escape", "http://127.0.0.1:1", "docker", ""); err == nil {
		t.Fatal("expected unsafe session id error")
	}
	if _, err := PrepareToolContainerProxy(t.TempDir(), "session-abc", "", "docker", ""); err == nil {
		t.Fatal("expected empty daemon root error")
	}
	if _, err := PrepareToolContainerProxy(t.TempDir(), "session-abc", "http://", "docker", ""); err == nil {
		t.Fatal("expected invalid daemon root error")
	}
}

func TestCleanupToolContainerProxy_RemovesSessionDirAndGuardsID(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := PrepareToolContainerProxy(stateDir, "session-abc", "http://127.0.0.1:1", "docker", "t"); err != nil {
		t.Fatalf("PrepareToolContainerProxy: %v", err)
	}
	dir := filepath.Join(stateDir, "sessions", "session-abc", "sandbox-proxy")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session dir must exist before cleanup: %v", err)
	}
	if err := CleanupToolContainerProxy(stateDir, "session-abc"); err != nil {
		t.Fatalf("CleanupToolContainerProxy: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir must be removed after cleanup, stat err = %v", err)
	}
	// An unsafe session id is rejected before any RemoveAll so the guard can
	// never direct the delete outside the sessions tree.
	if err := CleanupToolContainerProxy(stateDir, "../escape"); err == nil {
		t.Fatal("expected unsafe session id rejection")
	}
}

func TestWriteSessionCA_ReturnsCreateDirError(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeSessionCA(filepath.Join(parentFile, "ca.pem"), filepath.Join(parentFile, "ca.key"), "session-123")
	if err == nil {
		t.Fatal("expected create dir error")
	}
}

func TestApplySandboxProxyBootstrapEnv(t *testing.T) {
	env := map[string]string{"NO_PROXY": "internal.example"}
	applySandboxProxyBootstrapEnv(env, sandboxProxyBootstrap{
		Mode:     "bootstrap",
		ProxyURL: "http://session-123:daemon-token@host.docker.internal:48123",
	})

	for key, want := range map[string]string{
		"AILERON_SANDBOX_PROXY_MODE":    "bootstrap",
		"AILERON_SANDBOX_PROXY_URL":     "http://session-123:daemon-token@host.docker.internal:48123",
		"AILERON_SANDBOX_PROXY_CA_FILE": sandboxProxyCAPath,
		"HTTPS_PROXY":                   "http://session-123:daemon-token@host.docker.internal:48123",
		"HTTP_PROXY":                    "http://session-123:daemon-token@host.docker.internal:48123",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, want := range []string{"internal.example", "localhost", "127.0.0.1", "::1", "host.docker.internal"} {
		if !strings.Contains(env["NO_PROXY"], want) {
			t.Fatalf("NO_PROXY = %q, missing %q", env["NO_PROXY"], want)
		}
	}
}

func TestApplySandboxProxyBootstrapEnv_NoOpWhenBootstrapEmpty(t *testing.T) {
	env := map[string]string{"NO_PROXY": "internal.example"}
	applySandboxProxyBootstrapEnv(env, sandboxProxyBootstrap{})
	for key := range map[string]struct{}{
		"AILERON_SANDBOX_PROXY_MODE":    {},
		"AILERON_SANDBOX_PROXY_URL":     {},
		"AILERON_SANDBOX_PROXY_CA_FILE": {},
		"HTTPS_PROXY":                   {},
		"HTTP_PROXY":                    {},
	} {
		if _, ok := env[key]; ok {
			t.Fatalf("env[%s] should not be set when bootstrap is empty: %v", key, env)
		}
	}
	if env["NO_PROXY"] != "internal.example" {
		t.Fatalf("NO_PROXY mutated: %q", env["NO_PROXY"])
	}
}

func TestSandboxProxyURLWithSessionAuth_NoDaemonToken(t *testing.T) {
	got, err := sandboxProxyURLWithSessionAuth("http://host.docker.internal:48123", "session-123", "")
	if err != nil {
		t.Fatalf("sandboxProxyURLWithSessionAuth: %v", err)
	}
	if got != "http://session-123@host.docker.internal:48123" {
		t.Fatalf("proxy URL = %q", got)
	}
}

func TestSandboxProxyURLWithSessionAuth_RejectsInvalidURL(t *testing.T) {
	if _, err := sandboxProxyURLWithSessionAuth("host.docker.internal:48123", "session-123", "daemon-token"); err == nil {
		t.Fatal("expected invalid proxy URL error")
	}
}

// TestResolveSandboxProxyState walks the canonical resolution matrix
// from the U3 plan. The table form keeps the precedence rules
// (flag > env > default) and the sandbox-mode dependency obvious in
// one place; downstream launcher behavior follows from these results
// without a second source of truth.
func TestResolveSandboxProxyState(t *testing.T) {
	cases := []struct {
		name       string
		flag       string
		env        string
		mode       string
		wantOn     bool
		wantRefuse bool
		wantReason string
	}{
		// docker defaults: auto/empty/"on" → enabled.
		{name: "docker_flag_auto_env_unset", flag: "auto", env: "", mode: "docker", wantOn: true},
		{name: "docker_flag_empty_env_unset", flag: "", env: "", mode: "docker", wantOn: true},
		{name: "docker_flag_on", flag: "on", env: "", mode: "docker", wantOn: true},
		{name: "docker_env_on", flag: "", env: "on", mode: "docker", wantOn: true},

		// docker opt-out: flag=off or env=off → user_opt_out.
		{name: "docker_flag_off", flag: "off", env: "", mode: "docker", wantOn: false, wantReason: sandboxProxyReasonUserOptOut},
		{name: "docker_env_off", flag: "auto", env: "off", mode: "docker", wantOn: false, wantReason: sandboxProxyReasonUserOptOut},
		{name: "docker_env_off_no_flag", flag: "", env: "off", mode: "docker", wantOn: false, wantReason: sandboxProxyReasonUserOptOut},

		// Flag-vs-env precedence: flag wins.
		{name: "flag_on_env_off", flag: "on", env: "off", mode: "docker", wantOn: true},
		{name: "flag_off_env_on", flag: "off", env: "on", mode: "docker", wantOn: false, wantReason: sandboxProxyReasonUserOptOut},

		// Non-container modes: auto/off → unsupported_sandbox_mode disabled.
		{name: "off_mode_auto", flag: "auto", env: "", mode: "off", wantOn: false, wantReason: sandboxProxyReasonUnsupportedSandboxMode},
		{name: "empty_mode_auto", flag: "auto", env: "", mode: "", wantOn: false, wantReason: sandboxProxyReasonUnsupportedSandboxMode},
		{name: "off_mode_env_off", flag: "", env: "off", mode: "off", wantOn: false, wantReason: sandboxProxyReasonUnsupportedSandboxMode},

		// Non-container modes with flag=on: refuse with unsupported_sandbox_mode.
		{name: "off_mode_flag_on", flag: "on", env: "", mode: "off", wantOn: false, wantRefuse: true, wantReason: sandboxProxyReasonUnsupportedSandboxMode},
		{name: "empty_mode_flag_on", flag: "on", env: "", mode: "", wantOn: false, wantRefuse: true, wantReason: sandboxProxyReasonUnsupportedSandboxMode},

		// Unknown flag/env values fall back to default behavior.
		{name: "docker_garbage_flag_auto", flag: "garbage", env: "", mode: "docker", wantOn: true},
		{name: "docker_garbage_env_default", flag: "", env: "garbage", mode: "docker", wantOn: true},

		// Legacy AILERON_SANDBOX_PROXY_BOOTSTRAP-style values are not
		// honored by this resolver; the launcher only consults
		// AILERON_SANDBOX_PROXY. Verified separately via the env name
		// constant test below.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSandboxProxyState(tc.flag, tc.env, tc.mode)
			if got.Enabled != tc.wantOn {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tc.wantOn)
			}
			if got.Refuse != tc.wantRefuse {
				t.Errorf("Refuse = %v, want %v", got.Refuse, tc.wantRefuse)
			}
			if got.DisabledReason != tc.wantReason {
				t.Errorf("DisabledReason = %q, want %q", got.DisabledReason, tc.wantReason)
			}
		})
	}
}

// TestResolveSandboxProxyState_EnvVarName guards the rename from
// AILERON_SANDBOX_PROXY_BOOTSTRAP to AILERON_SANDBOX_PROXY. The new
// name governs; the old name is no longer recognized. Verified at the
// constant layer so a future refactor that changes the constant
// without updating the resolver code paths still fails this test.
func TestResolveSandboxProxyState_EnvVarName(t *testing.T) {
	if sandboxProxyEnv != "AILERON_SANDBOX_PROXY" {
		t.Fatalf("sandbox proxy env var = %q, want AILERON_SANDBOX_PROXY", sandboxProxyEnv)
	}
}
