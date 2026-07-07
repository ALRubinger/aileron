package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

// setMetadataLabel swaps the containerImageMetadataLabel seam to return a fixed
// label for the duration of the test, so the plan bootstrapper never shells out
// to `docker image inspect`.
func setMetadataLabel(t *testing.T, label string) {
	t.Helper()
	prev := containerImageMetadataLabel
	containerImageMetadataLabel = func(_ context.Context, _, _ string) string { return label }
	t.Cleanup(func() { containerImageMetadataLabel = prev })
}

func stubEnsureImageLocal(t *testing.T, fn func(context.Context, string, string) error) {
	t.Helper()
	prev := containerEnsureImageLocal
	containerEnsureImageLocal = fn
	t.Cleanup(func() { containerEnsureImageLocal = prev })
}

// twoToolMetadata is a devcontainer.metadata label carrying the aws-cli and gh
// credential conventions, the multi-tool union the boot must plant.
const twoToolMetadata = `[
  {"id":"aws-cli","customizations":{"aileron":{"credential":{"scheme":"sigv4-resign","placeholders":[{"env":"AWS_ACCESS_KEY_ID","value":"AKIAIOSFODNN7PLACEHLDR"},{"env":"AWS_SECRET_ACCESS_KEY","value":"placeholderAileronInjectsRealSecretXXXXXX"}]}}}},
  {"id":"gh","customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[{"env":"GH_TOKEN","value":"ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"}]}}}}
]`

func TestDaemonPlanProxyBootstrapper_AssemblesExactEnvAndCAFromDiscovery(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")
	setMetadataLabel(t, twoToolMetadata)
	stubEnsureImageLocal(t, func(context.Context, string, string) error { return nil })

	bootstrap, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "example.com/plan@sha256:abc", "myplan")
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
	// ca.pem exists on disk under the boot session dir.
	if _, err := os.Stat(bootstrap.Mount.Source); err != nil {
		t.Fatalf("CA not written on disk: %v", err)
	}
	if !strings.Contains(bootstrap.Mount.Source, filepath.Join("sessions", "flightplan-boot-myplan-")) {
		t.Errorf("CA source %q not under the flightplan-boot session dir", bootstrap.Mount.Source)
	}

	// The env is EXACTLY the expected map — the authed loopback-rewritten proxy
	// URL, the six CA vars, NO_PROXY/no_proxy, and the aws-cli + gh placeholder
	// union — and NOTHING ELSE. Exact-map equality is the "never a real secret"
	// assertion at this layer.
	proxyURL := bootstrap.Env["HTTPS_PROXY"]
	want := map[string]string{
		"HTTPS_PROXY":           proxyURL,
		"https_proxy":           proxyURL,
		"NO_PROXY":              "localhost,127.0.0.1,::1,host.docker.internal",
		"no_proxy":              "localhost,127.0.0.1,::1,host.docker.internal",
		"AWS_CA_BUNDLE":         "/etc/aileron/proxy/ca.pem",
		"REQUESTS_CA_BUNDLE":    "/etc/aileron/proxy/ca.pem",
		"NODE_EXTRA_CA_CERTS":   "/etc/aileron/proxy/ca.pem",
		"SSL_CERT_FILE":         "/etc/aileron/proxy/ca.pem",
		"GIT_SSL_CAINFO":        "/etc/aileron/proxy/ca.pem",
		"CURL_CA_BUNDLE":        "/etc/aileron/proxy/ca.pem",
		"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7PLACEHLDR",
		"AWS_SECRET_ACCESS_KEY": "placeholderAileronInjectsRealSecretXXXXXX",
		"GH_TOKEN":              "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA",
	}
	if !reflect.DeepEqual(bootstrap.Env, want) {
		t.Fatalf("boot env = %#v\nwant %#v", bootstrap.Env, want)
	}

	// The proxy URL is loopback-rewritten to host.docker.internal and carries the
	// session:token userinfo the CONNECT handshake authenticates.
	if !strings.Contains(proxyURL, "host.docker.internal:48123") {
		t.Errorf("HTTPS_PROXY = %q, want host.docker.internal rewrite", proxyURL)
	}
	if !strings.Contains(proxyURL, ":daemon-token@") {
		t.Errorf("HTTPS_PROXY = %q, want :daemon-token@ userinfo", proxyURL)
	}

	// The real daemon token is a proxy-auth credential, not a network secret; it
	// must appear ONLY in the proxy URL userinfo, never leaked into a CA-bundle
	// or placeholder var.
	for _, key := range append(append([]string{}, caBundleEnvVars...), "AWS_SECRET_ACCESS_KEY", "GH_TOKEN") {
		if strings.Contains(bootstrap.Env[key], "daemon-token") {
			t.Errorf("%s leaked the daemon token: %q", key, bootstrap.Env[key])
		}
	}

	// Cleanup removes the boot session CA directory.
	cleanup()
	if _, err := os.Stat(bootstrap.Mount.Source); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the session CA, stat err = %v", err)
	}
}

func TestDaemonPlanProxyBootstrapper_PrefersEnvOverDiscovery(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	// Discovery carries a different URL/token; the env must win.
	writeDiscovery(t, stateDir, "http://127.0.0.1:1/", "discovery-token")
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:55555/v1")
	t.Setenv("AILERON_TOKEN", "env-token")
	setMetadataLabel(t, "")
	stubEnsureImageLocal(t, func(context.Context, string, string) error { return nil })

	bootstrap, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
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

func TestDaemonPlanProxyBootstrapper_EmptyMetadataLabelYieldsNoPlaceholders(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")
	setMetadataLabel(t, "") // unlabeled / uninspectable image
	stubEnsureImageLocal(t, func(context.Context, string, string) error { return nil })

	bootstrap, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
	if err != nil {
		t.Fatalf("Prepare must fail-soft on an empty label: %v", err)
	}
	if !ok {
		t.Fatal("Prepare must still enrich proxy env + CA on an empty label")
	}
	t.Cleanup(cleanup)

	// Proxy + CA are present, but no placeholder creds.
	if bootstrap.Env["HTTPS_PROXY"] == "" {
		t.Error("HTTPS_PROXY must still be set with an empty metadata label")
	}
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GH_TOKEN"} {
		if _, present := bootstrap.Env[k]; present {
			t.Errorf("placeholder %s must be absent when the label carries no conventions", k)
		}
	}
}

func TestDaemonPlanProxyBootstrapper_MalformedMetadataFailsClosed(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")
	// A present-but-invalid credential block (unknown scheme) must refuse the
	// boot rather than silently ship no placeholders.
	setMetadataLabel(t, `[{"customizations":{"aileron":{"credential":{"scheme":"nope","placeholders":[{"env":"X","value":"y"}]}}}}]`)
	stubEnsureImageLocal(t, func(context.Context, string, string) error { return nil })

	_, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
	if err == nil {
		t.Fatal("Prepare must error on a malformed metadata label")
	}
	if ok {
		t.Fatal("Prepare must not report ok on the fail-closed path")
	}
	// The error path still returns a runnable cleanup so the partial CA is
	// removed rather than leaked.
	if cleanup == nil {
		t.Fatal("Prepare must return a runnable cleanup on the error path")
	}
	cleanup()
}

func TestDaemonPlanProxyBootstrapper_ReservedKeysWinOverPlaceholders(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")
	// A (mis)declared placeholder trying to claim a reserved proxy/CA env must
	// NOT clobber the authoritative value — reserved keys always win, so the
	// fail-closed egress mediation cannot be weakened by image metadata.
	setMetadataLabel(t, `[
  {"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[
    {"env":"HTTPS_PROXY","value":"http://evil.example"},
    {"env":"NO_PROXY","value":"*"},
    {"env":"AWS_CA_BUNDLE","value":"/tmp/evil.pem"},
    {"env":"GH_TOKEN","value":"ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"}
  ]}}}}
]`)
	stubEnsureImageLocal(t, func(context.Context, string, string) error { return nil })

	bootstrap, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !ok {
		t.Fatal("Prepare must resolve")
	}
	t.Cleanup(cleanup)

	if strings.Contains(bootstrap.Env["HTTPS_PROXY"], "evil.example") {
		t.Errorf("HTTPS_PROXY = %q, a placeholder must never override the authoritative proxy URL", bootstrap.Env["HTTPS_PROXY"])
	}
	if bootstrap.Env["NO_PROXY"] != "localhost,127.0.0.1,::1,host.docker.internal" {
		t.Errorf("NO_PROXY = %q, a placeholder must never override the reserved bypass list", bootstrap.Env["NO_PROXY"])
	}
	if bootstrap.Env["AWS_CA_BUNDLE"] != "/etc/aileron/proxy/ca.pem" {
		t.Errorf("AWS_CA_BUNDLE = %q, a placeholder must never override the mounted CA path", bootstrap.Env["AWS_CA_BUNDLE"])
	}
	// A non-reserved placeholder (GH_TOKEN) still lands.
	if bootstrap.Env["GH_TOKEN"] != "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("GH_TOKEN = %q, a non-reserved placeholder must still be planted", bootstrap.Env["GH_TOKEN"])
	}
}

func TestDaemonPlanProxyBootstrapper_ConflictingPlaceholdersFailClosed(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")
	// Two elements declare GH_TOKEN with different values: the union is a
	// conflict and the boot must refuse rather than last-wins-ship a placeholder.
	setMetadataLabel(t, `[
  {"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[{"env":"GH_TOKEN","value":"ghp_a"}]}}}},
  {"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[{"env":"GH_TOKEN","value":"ghp_b"}]}}}}
]`)
	stubEnsureImageLocal(t, func(context.Context, string, string) error { return nil })

	_, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
	if err == nil {
		t.Fatal("Prepare must error on conflicting placeholder values")
	}
	if ok {
		t.Fatal("Prepare must not report ok on the fail-closed path")
	}
	if cleanup == nil {
		t.Fatal("Prepare must return a runnable cleanup on the error path")
	}
	cleanup()
}

func TestDaemonPlanProxyBootstrapper_PassthroughWhenConfigAbsent(t *testing.T) {
	t.Run("no daemon config at all", func(t *testing.T) {
		withHomeAndEnv(t) // empty state dir, no env
		setMetadataLabel(t, twoToolMetadata)
		_, _, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
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
		setMetadataLabel(t, twoToolMetadata)
		_, _, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
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
		setMetadataLabel(t, twoToolMetadata)
		_, _, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "img", "p")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if ok {
			t.Fatal("Prepare must be passthrough when the daemon URL is absent")
		}
	})
}

func TestDaemonPlanProxyBootstrapper_MaterializesImageBeforeReadingMetadata(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")

	var calls []string
	stubEnsureImageLocal(t, func(_ context.Context, runtimeName, image string) error {
		calls = append(calls, "ensure:"+runtimeName+":"+image)
		return nil
	})
	prevLabel := containerImageMetadataLabel
	containerImageMetadataLabel = func(_ context.Context, runtimeName, image string) string {
		calls = append(calls, "metadata:"+runtimeName+":"+image)
		return twoToolMetadata
	}
	t.Cleanup(func() { containerImageMetadataLabel = prevLabel })

	bootstrap, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "registry.example.com/plan@sha256:abc", "p")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !ok {
		t.Fatal("Prepare must resolve")
	}
	t.Cleanup(cleanup)
	if bootstrap.Env["AWS_ACCESS_KEY_ID"] == "" {
		t.Fatal("placeholder env must be derived after the image is materialized")
	}
	want := []string{
		"ensure:docker:registry.example.com/plan@sha256:abc",
		"metadata:docker:registry.example.com/plan@sha256:abc",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("call order = %#v, want %#v", calls, want)
	}
}

func TestDaemonPlanProxyBootstrapper_MaterializeFailureFailsBeforeCA(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	writeDiscovery(t, stateDir, "http://127.0.0.1:48123", "daemon-token")

	metadataRead := false
	prevLabel := containerImageMetadataLabel
	containerImageMetadataLabel = func(context.Context, string, string) string {
		metadataRead = true
		return twoToolMetadata
	}
	t.Cleanup(func() { containerImageMetadataLabel = prevLabel })
	stubEnsureImageLocal(t, func(context.Context, string, string) error {
		return errors.New("pull denied")
	})

	_, cleanup, ok, err := daemonPlanProxyBootstrapper{}.Prepare(context.Background(), "docker", "registry.example.com/plan@sha256:abc", "p")
	if err == nil {
		t.Fatal("Prepare must fail when the pinned image cannot be materialized")
	}
	if ok {
		t.Fatal("Prepare must not report ok on materialize failure")
	}
	if cleanup != nil {
		t.Fatal("materialize failure happens before CA provisioning, so no cleanup is needed")
	}
	if metadataRead {
		t.Fatal("metadata must not be read after materialize failure")
	}
	for _, want := range []string{"materialize pinned image", "registry.example.com/plan@sha256:abc", "pull denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestEnsureImageLocal_SkipsPullWhenInspectFindsImage(t *testing.T) {
	var calls []string
	runner := runnerFunc(func(_ context.Context, name string, args []string, _, _ io.Writer) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	})

	if err := ensureImageLocal(context.Background(), runner, "docker", "registry.example.com/plan@sha256:abc"); err != nil {
		t.Fatalf("ensureImageLocal: %v", err)
	}
	want := []string{"docker image inspect registry.example.com/plan@sha256:abc"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureImageLocal_PullsWhenInspectMisses(t *testing.T) {
	var calls []string
	runner := runnerFunc(func(_ context.Context, name string, args []string, _, _ io.Writer) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if args[0] == "image" {
			return errors.New("not local")
		}
		return nil
	})

	if err := ensureImageLocal(context.Background(), runner, "docker", "registry.example.com/plan@sha256:abc"); err != nil {
		t.Fatalf("ensureImageLocal: %v", err)
	}
	want := []string{
		"docker image inspect registry.example.com/plan@sha256:abc",
		"docker pull registry.example.com/plan@sha256:abc",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureImageLocal_PullFailureNamesRuntimeAndImage(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, args []string, _, _ io.Writer) error {
		if args[0] == "image" {
			return errors.New("not local")
		}
		return errors.New("denied")
	})

	err := ensureImageLocal(context.Background(), runner, "docker", "registry.example.com/plan@sha256:abc")
	if err == nil {
		t.Fatal("ensureImageLocal must return the pull error")
	}
	for _, want := range []string{"docker pull", "registry.example.com/plan@sha256:abc", "denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
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
		"":           "plan",
		"with space": "with-space",
	}
	for in, want := range cases {
		if got := sanitizeSessionSegment(in); got != want {
			t.Errorf("sanitizeSessionSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDaemonImageEnv_RewritesHostAndAppendsV1FromEnv(t *testing.T) {
	withHomeAndEnv(t)
	// A loopback AILERON_API_URL carrying the /v1 API prefix, no daemon spawn.
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:48123/v1")
	t.Setenv("AILERON_TOKEN", "env-token")

	env, ok := daemonImageEnv{}.Env("docker")
	if !ok {
		t.Fatal("Env must resolve when URL + token are set")
	}
	// The injected URL is host.docker.internal-rewritten and carries the /v1
	// suffix the inner bindingAPIBaseURL uses directly as base + "/audit".
	if got := env["AILERON_API_URL"]; got != "http://host.docker.internal:48123/v1" {
		t.Errorf("AILERON_API_URL = %q, want host.docker.internal-rewritten /v1 URL", got)
	}
	if got := env["AILERON_TOKEN"]; got != "env-token" {
		t.Errorf("AILERON_TOKEN = %q, want the resolved daemon token", got)
	}
}

func TestDaemonImageEnv_ResolvesFromDiscovery(t *testing.T) {
	stateDir := withHomeAndEnv(t)
	// Discovery carries a loopback root URL (no /v1) + token; no daemon spawn.
	writeDiscovery(t, stateDir, "http://localhost:55555", "discovery-token")

	env, ok := daemonImageEnv{}.Env("docker")
	if !ok {
		t.Fatal("Env must resolve from discovery when env is unset")
	}
	if got := env["AILERON_API_URL"]; got != "http://host.docker.internal:55555/v1" {
		t.Errorf("AILERON_API_URL = %q, want host.docker.internal-rewritten /v1 URL", got)
	}
	if got := env["AILERON_TOKEN"]; got != "discovery-token" {
		t.Errorf("AILERON_TOKEN = %q, want the discovery token", got)
	}
}

func TestDaemonImageEnv_NonLoopbackHostUnchanged(t *testing.T) {
	withHomeAndEnv(t)
	// A non-loopback host must not be rewritten, but must still carry /v1.
	t.Setenv("AILERON_API_URL", "https://daemon.internal.example:8721")
	t.Setenv("AILERON_TOKEN", "t")

	env, ok := daemonImageEnv{}.Env("docker")
	if !ok {
		t.Fatal("Env must resolve for a non-loopback host")
	}
	if got := env["AILERON_API_URL"]; got != "https://daemon.internal.example:8721/v1" {
		t.Errorf("AILERON_API_URL = %q, want the non-loopback host unchanged with /v1", got)
	}
}

func TestDaemonImageEnv_NoURLPassthrough(t *testing.T) {
	withHomeAndEnv(t)
	// Neither env nor discovery yields a URL: passthrough (no injected env).
	t.Setenv("AILERON_TOKEN", "t")

	if env, ok := (daemonImageEnv{}).Env("docker"); ok || env != nil {
		t.Errorf("Env = (%v, %v), want (nil, false) when no URL resolves", env, ok)
	}
}

func TestDaemonImageEnv_NoTokenPassthrough(t *testing.T) {
	withHomeAndEnv(t)
	// A URL but no token: the daemon action/audit POST would be unauthenticated,
	// so the resolver stays passthrough.
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:48123/v1")

	if env, ok := (daemonImageEnv{}).Env("docker"); ok || env != nil {
		t.Errorf("Env = (%v, %v), want (nil, false) when no token resolves", env, ok)
	}
}

func TestDaemonImageAPIURL(t *testing.T) {
	cases := map[string]string{
		// loopback roots (no /v1) are rewritten and get /v1 appended
		"http://127.0.0.1:48123":  "http://host.docker.internal:48123/v1",
		"http://localhost:48123":  "http://host.docker.internal:48123/v1",
		"http://127.0.0.1:48123/": "http://host.docker.internal:48123/v1",
		// non-loopback host is left as-is, /v1 appended
		"https://daemon.example:8721": "https://daemon.example:8721/v1",
	}
	for in, want := range cases {
		if got := daemonImageAPIURL(in, "docker"); got != want {
			t.Errorf("daemonImageAPIURL(%q) = %q, want %q", in, got, want)
		}
	}
}
