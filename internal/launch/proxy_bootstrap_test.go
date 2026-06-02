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
	got, err := prepareSandboxProxyBootstrap(t.TempDir(), "session-123", "http://host.docker.internal:48123")
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
	got, err := prepareSandboxProxyBootstrap(stateDir, "session-123", "http://host.docker.internal:48123/")
	if err != nil {
		t.Fatalf("prepareSandboxProxyBootstrap: %v", err)
	}
	if got.Mode != "bootstrap" {
		t.Fatalf("Mode = %q, want bootstrap", got.Mode)
	}
	if got.ProxyURL != "http://host.docker.internal:48123" {
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

func TestApplySandboxProxyBootstrapEnv(t *testing.T) {
	env := map[string]string{"NO_PROXY": "internal.example"}
	applySandboxProxyBootstrapEnv(env, sandboxProxyBootstrap{
		Mode:     "bootstrap",
		ProxyURL: "http://host.docker.internal:48123",
	})

	for key, want := range map[string]string{
		"AILERON_SANDBOX_PROXY_MODE":    "bootstrap",
		"AILERON_SANDBOX_PROXY_URL":     "http://host.docker.internal:48123",
		"AILERON_SANDBOX_PROXY_CA_FILE": sandboxProxyCAPath,
		"HTTPS_PROXY":                   "http://host.docker.internal:48123",
		"HTTP_PROXY":                    "http://host.docker.internal:48123",
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
