package launch

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSandboxProxyBootstrap_DefaultOff(t *testing.T) {
	t.Setenv(sandboxProxyBootstrapEnv, "")
	got, err := prepareSandboxProxyBootstrap(t.TempDir(), "session-123", "http://host.docker.internal:48123", "")
	if err != nil {
		t.Fatalf("prepareSandboxProxyBootstrap: %v", err)
	}
	if got.Mode != "" || got.ProxyURL != "" || len(got.Mounts) != 0 {
		t.Fatalf("bootstrap = %+v, want zero value", got)
	}
}

func TestPrepareSandboxProxyBootstrap_GeneratesSessionCAAndMount(t *testing.T) {
	t.Setenv(sandboxProxyBootstrapEnv, "1")
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
	t.Setenv(sandboxProxyBootstrapEnv, "1")
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
	t.Setenv(sandboxProxyBootstrapEnv, "1")
	_, err := prepareSandboxProxyBootstrap(t.TempDir(), "session-123", "", "")
	if err == nil {
		t.Fatal("expected agent endpoint URL error")
	}
}

func TestPrepareSandboxProxyBootstrap_RejectsInvalidAgentEndpointURL(t *testing.T) {
	t.Setenv(sandboxProxyBootstrapEnv, "1")
	_, err := prepareSandboxProxyBootstrap(t.TempDir(), "session-123", "http://", "")
	if err == nil {
		t.Fatal("expected invalid agent endpoint URL error")
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
	for _, want := range []string{"internal.example", "localhost", "127.0.0.1", "::1", "host.docker.internal", "host.containers.internal"} {
		if !strings.Contains(env["NO_PROXY"], want) {
			t.Fatalf("NO_PROXY = %q, missing %q", env["NO_PROXY"], want)
		}
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
