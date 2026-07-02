package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
)

// writeDiscovery writes a daemon.json into stateDir so the bootstrapper's
// spawn-free config resolution has a URL + token to read.
func writeDiscovery(t *testing.T, stateDir, url, token string) {
	t.Helper()
	b, err := json.Marshal(discovery.Info{URL: url, Token: token, PID: 1234})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, discovery.InfoFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// withHomeAndEnv points defaultStateDir at a temp HOME and clears the
// AILERON_API_URL / AILERON_TOKEN env so a test controls exactly which config
// source resolves.
func withHomeAndEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("AILERON_API_URL", "")
	t.Setenv("AILERON_TOKEN", "")
	stateDir := filepath.Join(home, ".aileron")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

func TestDaemonToolProxyBootstrapper_AssemblesEnvAndCAFromDiscovery(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")

	bootstrap, cleanup, ok, err := daemonToolProxyBootstrapper{}.Prepare("docker", "extract")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !ok {
		t.Fatal("Prepare must resolve when discovery carries URL + token")
	}
	t.Cleanup(cleanup)

	// The CA mount is read-only at the well-known in-container CA path.
	if bootstrap.Mount.Target != "/etc/aileron/proxy/ca.pem" || !bootstrap.Mount.ReadOnly {
		t.Fatalf("CA mount = %+v, want RO at /etc/aileron/proxy/ca.pem", bootstrap.Mount)
	}
	// The CA exists on disk under the session dir.
	if _, err := os.Stat(bootstrap.Mount.Source); err != nil {
		t.Fatalf("CA not written on disk: %v", err)
	}

	// HTTPS_PROXY (both cases) is loopback-rewritten to host.docker.internal and
	// carries the session:token userinfo the CONNECT handshake authenticates.
	for _, key := range []string{"HTTPS_PROXY", "https_proxy"} {
		got := bootstrap.Env[key]
		if !strings.Contains(got, "host.docker.internal:48123") {
			t.Errorf("%s = %q, want host.docker.internal rewrite", key, got)
		}
		if !strings.Contains(got, ":daemon-token@") {
			t.Errorf("%s = %q, want :daemon-token@ userinfo", key, got)
		}
	}

	// Every CA-bundle var points at the mounted CA path.
	for _, key := range caBundleEnvVars {
		if got := bootstrap.Env[key]; got != "/etc/aileron/proxy/ca.pem" {
			t.Errorf("%s = %q, want the container CA path", key, got)
		}
	}

	// Placeholder (non-secret) AWS creds are seeded so botocore pre-signs.
	if bootstrap.Env["AWS_ACCESS_KEY_ID"] != placeholderAWSAccessKeyID {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want placeholder", bootstrap.Env["AWS_ACCESS_KEY_ID"])
	}
	if bootstrap.Env["AWS_SECRET_ACCESS_KEY"] != placeholderAWSSecretKey {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want placeholder", bootstrap.Env["AWS_SECRET_ACCESS_KEY"])
	}

	// The real daemon token is a proxy-auth credential, not a network secret; it
	// must appear ONLY in the proxy URL userinfo, never leaked into a CA-bundle
	// or credential var.
	for _, key := range append(append([]string{}, caBundleEnvVars...), "AWS_SECRET_ACCESS_KEY") {
		if strings.Contains(bootstrap.Env[key], "daemon-token") {
			t.Errorf("%s leaked the daemon token: %q", key, bootstrap.Env[key])
		}
	}

	// Cleanup removes the session CA directory.
	cleanup()
	if _, err := os.Stat(bootstrap.Mount.Source); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the session CA, stat err = %v", err)
	}
}

func TestDaemonToolProxyBootstrapper_PrefersEnvOverDiscovery(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	// Discovery carries a different URL/token; the env must win.
	writeDiscovery(t, stateDir, "http://127.0.0.1:1/", "discovery-token")
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:55555/v1")
	t.Setenv("AILERON_TOKEN", "env-token")

	bootstrap, cleanup, ok, err := daemonToolProxyBootstrapper{}.Prepare("docker", "s1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !ok {
		t.Fatal("Prepare must resolve from env")
	}
	t.Cleanup(cleanup)
	// The /v1 suffix is stripped to get the daemon root the proxy CONNECT is on.
	got := bootstrap.Env["HTTPS_PROXY"]
	if !strings.Contains(got, "host.docker.internal:55555") {
		t.Errorf("HTTPS_PROXY = %q, want env host:port", got)
	}
	if strings.Contains(got, "/v1") {
		t.Errorf("HTTPS_PROXY = %q, must not carry the /v1 API suffix", got)
	}
	if !strings.Contains(got, ":env-token@") {
		t.Errorf("HTTPS_PROXY = %q, want env token userinfo", got)
	}
}

func TestDaemonToolProxyBootstrapper_PassthroughWhenConfigAbsent(t *testing.T) {
	t.Run("no daemon config at all", func(t *testing.T) {
		withHomeAndEnv(t) // empty state dir, no env
		_, _, ok, err := daemonToolProxyBootstrapper{}.Prepare("docker", "s1")
		if err != nil {
			t.Fatalf("Prepare must not error on absent config: %v", err)
		}
		if ok {
			t.Fatal("Prepare must be passthrough (ok=false) when no daemon config resolves")
		}
	})

	t.Run("url present but token absent", func(t *testing.T) {
		stateDir := withHomeAndEnv(t)
		writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "") // no token
		_, _, ok, err := daemonToolProxyBootstrapper{}.Prepare("docker", "s1")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if ok {
			t.Fatal("Prepare must be passthrough when the CONNECT token is absent")
		}
	})

	t.Run("token present but url absent", func(t *testing.T) {
		withHomeAndEnv(t)
		t.Setenv("AILERON_TOKEN", "env-token") // token but no URL / discovery
		_, _, ok, err := daemonToolProxyBootstrapper{}.Prepare("docker", "s1")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if ok {
			t.Fatal("Prepare must be passthrough when the daemon URL is absent")
		}
	})
}

func TestStripV1Suffix(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:48123/v1":  "http://127.0.0.1:48123",
		"http://127.0.0.1:48123/v1/": "http://127.0.0.1:48123",
		"http://127.0.0.1:48123":     "http://127.0.0.1:48123",
		"http://127.0.0.1:48123/":    "http://127.0.0.1:48123",
	}
	for in, want := range cases {
		if got := stripV1Suffix(in); got != want {
			t.Errorf("stripV1Suffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeSessionSegment(t *testing.T) {
	cases := map[string]string{
		"extract":    "extract",
		"step_1-2":   "step_1-2",
		"a/b":        "a-b",
		"../escape":  "---escape",
		"":           "step",
		"with space": "with-space",
	}
	for in, want := range cases {
		if got := sanitizeSessionSegment(in); got != want {
			t.Errorf("sanitizeSessionSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
