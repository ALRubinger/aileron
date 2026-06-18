package launch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

type emptyBinaryAgent struct{}

func (emptyBinaryAgent) Name() string           { return "empty" }
func (emptyBinaryAgent) BinaryNames() []string  { return nil }
func (emptyBinaryAgent) Args() []string         { return nil }
func (emptyBinaryAgent) Env() map[string]string { return nil }
func (emptyBinaryAgent) LLMEndpointEnv() string { return "" }
func (emptyBinaryAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (emptyBinaryAgent) AuthSpec() AuthSpec { return AuthSpec{} }

type namedBinaryAgent struct{ name string }

func (a namedBinaryAgent) Name() string           { return "named" }
func (a namedBinaryAgent) BinaryNames() []string  { return []string{a.name} }
func (a namedBinaryAgent) Args() []string         { return nil }
func (a namedBinaryAgent) Env() map[string]string { return nil }
func (a namedBinaryAgent) LLMEndpointEnv() string { return "" }
func (a namedBinaryAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (a namedBinaryAgent) AuthSpec() AuthSpec { return AuthSpec{} }

func TestFirstAgentBinaryHandlesEmptyAgent(t *testing.T) {
	if got := firstAgentBinary(emptyBinaryAgent{}); got != "" {
		t.Fatalf("firstAgentBinary = %q, want empty", got)
	}
	if got := firstAgentBinary(namedBinaryAgent{name: "codex"}); got != "codex" {
		t.Fatalf("firstAgentBinary = %q, want codex", got)
	}
}

func TestContainerURLForRuntimeRewritesLoopbackAliases(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		runtime string
		want    string
	}{
		{
			name:    "docker localhost",
			rawURL:  "http://localhost:48123",
			runtime: "docker",
			want:    "http://host.docker.internal:48123",
		},
		{
			// v4 is Docker-only: any non-docker runtime threaded
			// through the preserved runtimeName seam still rewrites
			// the loopback host to host.docker.internal.
			name:    "non-docker runtime rewrites to docker host",
			rawURL:  "http://127.0.0.1:48123",
			runtime: "nondocker",
			want:    "http://host.docker.internal:48123",
		},
		{
			name:    "non-loopback unchanged",
			rawURL:  "http://10.0.0.5:48123",
			runtime: "docker",
			want:    "http://10.0.0.5:48123",
		},
		{
			name:    "localhost without port",
			rawURL:  "http://localhost",
			runtime: "docker",
			want:    "http://host.docker.internal",
		},
		{
			name:    "invalid unchanged",
			rawURL:  "://not-a-url",
			runtime: "docker",
			want:    "://not-a-url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerURLForRuntime(tt.rawURL, tt.runtime); got != tt.want {
				t.Fatalf("containerURLForRuntime(%q, %q) = %q, want %q", tt.rawURL, tt.runtime, got, tt.want)
			}
		})
	}
}

func TestDaemonAPIBaseURLAppendsV1(t *testing.T) {
	tests := map[string]string{
		"http://host.docker.internal:48123":  "http://host.docker.internal:48123/v1",
		"http://host.docker.internal:48123/": "http://host.docker.internal:48123/v1",
	}
	for input, want := range tests {
		if got := daemonAPIBaseURL(input); got != want {
			t.Fatalf("daemonAPIBaseURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLaunchSandboxRejectsAgentWithoutBinary(t *testing.T) {
	_, err := launchSandbox(nil, SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: emptyBinaryAgent{},
	}, nil, "aileron-sbx-test", func() {}, nil)
	if err == nil {
		t.Fatal("expected missing container command error")
	}
}

func TestValidateSandboxRejectsAgentWithoutBinary(t *testing.T) {
	err := validateSandbox(nil, SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: emptyBinaryAgent{},
	}, nil)
	if err == nil {
		t.Fatal("expected missing container command error")
	}
}

func TestValidateSandboxPassesRuntimeMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "send-email.md"), []byte(validActionManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := validateSandboxImageForLaunch
	t.Cleanup(func() { validateSandboxImageForLaunch = orig })
	var got []sandboxcontainer.Volume
	validateSandboxImageForLaunch = func(_ context.Context, _ SandboxLaunchPlan, _ LaunchConfig, _ map[string]string, mounts []sandboxcontainer.Volume, commandName string) error {
		got = append(got, mounts...)
		if commandName != "codex" {
			t.Fatalf("commandName = %q, want codex", commandName)
		}
		return nil
	}

	err := validateSandbox(context.Background(), SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: namedBinaryAgent{name: "codex"},
		Dir:   t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("validateSandbox: %v", err)
	}
	if !hasMountTarget(got, "/opt/aileron/manifests/actions") {
		t.Fatalf("validateSandbox mounts = %#v, want manifests mount", got)
	}
	for _, absent := range []string{"/etc/aileron/tools.txt", "/usr/local/bin/google"} {
		if hasMountTarget(got, absent) {
			t.Fatalf("validateSandbox mounts = %#v, should not include retired shim surface %s", got, absent)
		}
	}
}

func TestValidateSandboxPassesExtraMountsAndEnv(t *testing.T) {
	orig := validateSandboxImageForLaunch
	t.Cleanup(func() { validateSandboxImageForLaunch = orig })
	agentEnv := map[string]string{"HTTPS_PROXY": "http://host.docker.internal:48123"}
	extraMount := sandboxcontainer.Volume{
		Source:   filepath.Join(t.TempDir(), "ca.pem"),
		Target:   sandboxProxyCAPath,
		ReadOnly: true,
	}
	var gotEnv map[string]string
	var gotMounts []sandboxcontainer.Volume
	validateSandboxImageForLaunch = func(_ context.Context, _ SandboxLaunchPlan, _ LaunchConfig, env map[string]string, mounts []sandboxcontainer.Volume, commandName string) error {
		gotEnv = env
		gotMounts = append(gotMounts, mounts...)
		if commandName != "codex" {
			t.Fatalf("commandName = %q, want codex", commandName)
		}
		return nil
	}

	err := validateSandbox(context.Background(), SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: namedBinaryAgent{name: "codex"},
		Dir:   t.TempDir(),
	}, agentEnv, extraMount)
	if err != nil {
		t.Fatalf("validateSandbox: %v", err)
	}
	if gotEnv["HTTPS_PROXY"] != "http://host.docker.internal:48123" {
		t.Fatalf("validation env = %#v", gotEnv)
	}
	if !hasMountTarget(gotMounts, sandboxProxyCAPath) {
		t.Fatalf("validation mounts = %#v, missing %s", gotMounts, sandboxProxyCAPath)
	}
}

// --- U4 / #957: baked-image host-mount skip ---

// fakeBaked swaps sandboxBakedMCPVersion to report the given version and
// restores it on cleanup. An empty version models an unbaked image.
func fakeBaked(t *testing.T, version string) {
	t.Helper()
	orig := sandboxBakedMCPVersion
	t.Cleanup(func() { sandboxBakedMCPVersion = orig })
	sandboxBakedMCPVersion = func(context.Context, string, string) string { return version }
}

func TestValidateSandboxBakedImageSkipsHostMount(t *testing.T) {
	fakeBaked(t, "0.0.42")
	orig := validateSandboxImageForLaunch
	t.Cleanup(func() { validateSandboxImageForLaunch = orig })
	var got []sandboxcontainer.Volume
	validateSandboxImageForLaunch = func(_ context.Context, _ SandboxLaunchPlan, _ LaunchConfig, _ map[string]string, mounts []sandboxcontainer.Volume, _ string) error {
		got = mounts
		return nil
	}

	// No host aileron-mcp is resolved on the baked path, so this passes even
	// when none exists on the host (the sealed-runtime case).
	err := validateSandbox(context.Background(), SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: namedBinaryAgent{name: "codex"},
		Dir:   t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("validateSandbox: %v", err)
	}
	if hasMountTarget(got, sandboxMCPBinPath) {
		t.Fatalf("baked image must not host-mount %s; mounts = %#v", sandboxMCPBinPath, got)
	}
}

func TestValidateSandboxUnbakedImageHostMounts(t *testing.T) {
	fakeBaked(t, "")
	orig := validateSandboxImageForLaunch
	t.Cleanup(func() { validateSandboxImageForLaunch = orig })
	var got []sandboxcontainer.Volume
	validateSandboxImageForLaunch = func(_ context.Context, _ SandboxLaunchPlan, _ LaunchConfig, _ map[string]string, mounts []sandboxcontainer.Volume, _ string) error {
		got = mounts
		return nil
	}

	err := validateSandbox(context.Background(), SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: namedBinaryAgent{name: "codex"},
		Dir:   t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("validateSandbox: %v", err)
	}
	if !hasMountTarget(got, sandboxMCPBinPath) {
		t.Fatalf("unbaked image must host-mount %s; mounts = %#v", sandboxMCPBinPath, got)
	}
}

// launchSandboxCapturedVolumes drives launchSandbox to a clean exit with the
// run/signal seams faked, returning the container volumes the run was given.
func launchSandboxCapturedVolumes(t *testing.T) []sandboxcontainer.Volume {
	t.Helper()
	origRun := sandboxRunContainer
	origNotify := sandboxSignalNotify
	origStop := sandboxSignalStop
	t.Cleanup(func() {
		sandboxRunContainer = origRun
		sandboxSignalNotify = origNotify
		sandboxSignalStop = origStop
	})
	var got []sandboxcontainer.Volume
	sandboxRunContainer = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		got = opts.Volumes
		return sandboxcontainer.RunResult{}, nil
	}
	sandboxSignalNotify = func(chan<- os.Signal, ...os.Signal) {}
	sandboxSignalStop = func(chan<- os.Signal) {}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := launchSandbox(context.Background(), SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: namedBinaryAgent{name: "claude"},
	}, map[string]string{}, "aileron-sbx-test", func() {}, logger)
	if err != nil {
		t.Fatalf("launchSandbox: %v", err)
	}
	return got
}

func TestLaunchSandboxBakedImageSkipsHostMount(t *testing.T) {
	fakeBaked(t, "0.0.42")
	got := launchSandboxCapturedVolumes(t)
	if hasMountTarget(got, sandboxMCPBinPath) {
		t.Fatalf("baked image must not host-mount %s; volumes = %#v", sandboxMCPBinPath, got)
	}
}

func TestLaunchSandboxUnbakedImageHostMounts(t *testing.T) {
	fakeBaked(t, "")
	got := launchSandboxCapturedVolumes(t)
	if !hasMountTarget(got, sandboxMCPBinPath) {
		t.Fatalf("unbaked image must host-mount %s; volumes = %#v", sandboxMCPBinPath, got)
	}
}

// TestValidateSandboxImageAlwaysRequiresMCPBinary locks the branch-agnostic
// invariant from KTD5: the validate smoke check runs against the in-image
// binary on baked images and the host-mounted binary on unbaked ones alike.
func TestValidateSandboxImageAlwaysRequiresMCPBinary(t *testing.T) {
	runner := &mcpRequireRecordingRunner{}
	err := (sandboxcontainer.Builder{Runtime: "docker", Runner: runner}).Validate(context.Background(), sandboxcontainer.ValidateOptions{
		Runtime:          "docker",
		Image:            "image:test",
		WorkDir:          t.TempDir(),
		Command:          []string{"claude"},
		RequireMCPBinary: true,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if runner.lastArg != "1" {
		t.Fatalf("RequireMCPBinary flag = %q, want 1", runner.lastArg)
	}
}

type mcpRequireRecordingRunner struct{ lastArg string }

func (r *mcpRequireRecordingRunner) Run(_ context.Context, _ string, args []string, _, _ io.Writer) error {
	if len(args) > 0 {
		r.lastArg = args[len(args)-1]
	}
	return nil
}

func TestNormalizeLaunchBuildPolicy(t *testing.T) {
	tests := map[string]string{
		"":       "auto",
		" auto ": "auto",
		"always": "always",
		"never":  "never",
	}
	for input, want := range tests {
		got, err := normalizeLaunchBuildPolicy(input)
		if err != nil {
			t.Fatalf("normalizeLaunchBuildPolicy(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeLaunchBuildPolicy(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := normalizeLaunchBuildPolicy("sometimes"); err == nil {
		t.Fatal("expected unsupported build policy error")
	}
}

func TestSandboxRuntimeMountsSkipsMissingStores(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := sandboxRuntimeMounts()
	if err != nil {
		t.Fatalf("sandboxRuntimeMounts: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("sandboxRuntimeMounts = %#v, want no mounts", got)
	}
}

func TestSandboxRuntimeMountsIncludesExistingStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	connectorsDir := filepath.Join(home, ".aileron", "store", "connectors")
	for _, dir := range []string{actionsDir, connectorsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mounts, err := sandboxRuntimeMounts()
	if err != nil {
		t.Fatalf("sandboxRuntimeMounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %#v, want two mounts", mounts)
	}
	if mounts[0].Source != actionsDir || mounts[0].Target != "/opt/aileron/manifests/actions" || !mounts[0].ReadOnly {
		t.Fatalf("actions mount = %#v", mounts[0])
	}
	if mounts[1].Source != connectorsDir || mounts[1].Target != "/opt/aileron/manifests/connectors" || !mounts[1].ReadOnly {
		t.Fatalf("connectors mount = %#v", mounts[1])
	}
}

// TestSandboxRuntimeMountsOmitsRetiredShimSurface pins the #959 removal:
// even with installed actions present, the runtime mounts expose only the
// read-only manifests directories. No tools.txt manifest and no
// /usr/local/bin shim are emitted; aileron-mcp is the sole tool surface.
func TestSandboxRuntimeMountsOmitsRetiredShimSurface(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "send-email.md"), []byte(validActionManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts, err := sandboxRuntimeMounts()
	if err != nil {
		t.Fatalf("sandboxRuntimeMounts: %v", err)
	}
	if !hasMountTarget(mounts, "/opt/aileron/manifests/actions") {
		t.Fatalf("mounts = %#v, want manifests mount", mounts)
	}
	if hasMountTarget(mounts, "/etc/aileron/tools.txt") {
		t.Fatalf("mounts = %#v, retired tools.txt should not be emitted", mounts)
	}
	for _, mount := range mounts {
		if strings.HasPrefix(mount.Target, "/usr/local/bin/") {
			t.Fatalf("mounts = %#v, retired shim mount %s should not be emitted", mounts, mount.Target)
		}
	}
}

func hasMountTarget(mounts []sandboxcontainer.Volume, target string) bool {
	for _, mount := range mounts {
		if mount.Target == target {
			return true
		}
	}
	return false
}

const validActionManifest = `+++
name = "send-email"
version = "1.0.0"
source = "hub://aileron/send-email@1.0.0"

[[requires.connectors]]
name = "github://ALRubinger/aileron-connector-google"
version = "1.0.0"
hash = "sha256:abc123"
capabilities = ["send"]

[match]
intent = "send email"

[[inputs]]
name = "to"
type = "string"
description = "recipient"

[[execute]]
id = "send"
connector = "github://ALRubinger/aileron-connector-google"
op = "send_email"
+++

# Send Email
`

// TestResolveSandboxMCPBinary_PrefersLinuxSibling locks in the
// precedence the launcher relies on under --sandbox=docker: when
// `task build:mcp` on a non-Linux host produces both `aileron-mcp`
// (host arch, unrunnable in a Linux container) and
// `aileron-mcp-linux-<arch>` (cross-compiled), the sandbox-flavored
// lookup must pick the Linux variant, even when both exist. This is the
// fix for the "aileron-mcp on PATH but not executable in this container
// (arch mismatch or corrupt mount)" failure mode.
func TestResolveSandboxMCPBinary_PrefersLinuxSibling(t *testing.T) {
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostBin := filepath.Join(dir, "aileron-mcp")
	if err := os.WriteFile(hostBin, []byte("host-arch binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	linuxBin := filepath.Join(dir, "aileron-mcp-linux-"+runtime.GOARCH)
	if err := os.WriteFile(linuxBin, []byte("linux/"+runtime.GOARCH+" binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSandboxMCPBinary(selfPath)
	if err != nil {
		t.Fatalf("resolveSandboxMCPBinary: %v", err)
	}
	want, _ := filepath.Abs(linuxBin)
	if got != want {
		t.Fatalf("got %q, want %q (the Linux-suffixed sibling)", got, want)
	}
}

// TestResolveSandboxMCPBinary_FallsBackToHostBinary covers the Linux
// host case: `task build:mcp` only produces the plain binary because it
// is already a Linux binary; the sandbox-flavored lookup must accept it.
func TestResolveSandboxMCPBinary_FallsBackToHostBinary(t *testing.T) {
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostBin := filepath.Join(dir, "aileron-mcp")
	if err := os.WriteFile(hostBin, []byte("host arch is linux"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSandboxMCPBinary(selfPath)
	if err != nil {
		t.Fatalf("resolveSandboxMCPBinary: %v", err)
	}
	want, _ := filepath.Abs(hostBin)
	if got != want {
		t.Fatalf("got %q, want %q (fallback to plain aileron-mcp)", got, want)
	}
}

// TestResolveSandboxMCPBinary_PropagatesMissingError ensures the
// "aileron-mcp not found" remediation hint surfaces even on the
// sandbox-flavored lookup, so users on a fresh checkout get the same
// `task build:mcp` pointer the host path already gives them.
func TestResolveSandboxMCPBinary_PropagatesMissingError(t *testing.T) {
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	_, err := resolveSandboxMCPBinary(selfPath)
	if err == nil {
		t.Fatal("expected error when neither variant exists")
	}
	if !strings.Contains(err.Error(), "aileron-mcp not found") || !strings.Contains(err.Error(), "task build:mcp") {
		t.Fatalf("error message lost the remediation hint: %v", err)
	}
}

// TestValidateSandboxRuntimeRejectsPodman is the launch-path regression
// test for #1051: --sandbox=podman must fail validation with the
// Docker-only message. The runtime seam (the runtimeName parameter)
// stays, but podman is no longer an accepted launch surface.
func TestValidateSandboxRuntimeRejectsPodman(t *testing.T) {
	err := validateSandboxRuntime("podman")
	if err == nil {
		t.Fatal("expected error for --sandbox=podman")
	}
	if want := `unsupported sandbox runtime "podman" (want off, auto, or docker)`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestValidateSandboxRuntimeAcceptsDockerOnlySurface pins the accepted
// launch runtimes after Podman removal.
func TestValidateSandboxRuntimeAcceptsDockerOnlySurface(t *testing.T) {
	for _, in := range []string{"", "off", sandboxcontainer.DefaultRuntime, "docker"} {
		if err := validateSandboxRuntime(in); err != nil {
			t.Fatalf("validateSandboxRuntime(%q) = %v, want nil", in, err)
		}
	}
}

// TestSandboxProxyModeSupportsBootstrapDockerOnly confirms proxy
// bootstrap support tracks the Docker-only surface: docker and auto are
// supported; podman is not.
func TestSandboxProxyModeSupportsBootstrapDockerOnly(t *testing.T) {
	for _, c := range []struct {
		mode string
		want bool
	}{
		{"docker", true},
		{sandboxcontainer.DefaultRuntime, true},
		{"podman", false},
		{"off", false},
		{"", false},
	} {
		if got := sandboxProxyModeSupportsBootstrap(c.mode); got != c.want {
			t.Fatalf("sandboxProxyModeSupportsBootstrap(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}

// nestedMountTarget reports the first volume target that sits strictly
// inside another volume's directory target — the failure mode behind
// issue #1143. It returns "" when no mount is nested. A file-inside-dir
// bind mount is what runc rejects under macOS Docker Desktop virtiofs.
func nestedMountTarget(mounts []sandboxcontainer.Volume) string {
	for i := range mounts {
		inner := path.Clean(mounts[i].Target)
		for j := range mounts {
			if i == j {
				continue
			}
			outer := path.Clean(mounts[j].Target)
			if strings.HasPrefix(inner, outer+"/") {
				return mounts[i].Target
			}
		}
	}
	return ""
}

// TestCollapseNestedMCPMounts_CollapsesFileIntoParentDir is the #1143
// regression: an MCP config file mount whose target is nested inside an
// existing writable directory mount (Codex's /home/agent/.codex/ +
// config.toml) must NOT survive as a separate nested bind mount. Instead
// the file is relocated into the directory mount's host-side source so a
// single directory mount carries both files. No bind mount may have a
// target strictly inside another mount's target.
func TestCollapseNestedMCPMounts_CollapsesFileIntoParentDir(t *testing.T) {
	dirSource := t.TempDir() // host-side source for /home/agent/.codex/
	cfgSource := filepath.Join(t.TempDir(), "config.toml")
	cfgBody := []byte("sandbox_mode = \"danger-full-access\"\ncli_auth_credentials_store = \"file\"\n\n[mcp_servers.aileron]\ncommand = \"/usr/local/bin/aileron-mcp\"\n")
	if err := os.WriteFile(cfgSource, cfgBody, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	existing := []sandboxcontainer.Volume{
		{Source: dirSource, Target: "/home/agent/.codex", ReadOnly: false},
	}
	mcp := []MCPMount{
		{Source: cfgSource, Target: "/home/agent/.codex/config.toml", ReadOnly: true},
	}

	got, err := collapseNestedMCPMounts(existing, mcp)
	if err != nil {
		t.Fatalf("collapseNestedMCPMounts: %v", err)
	}

	if n := nestedMountTarget(got); n != "" {
		t.Fatalf("nested bind mount survived collapse: %q in %+v", n, got)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 collapsed mount, got %d: %+v", len(got), got)
	}
	if got[0].Target != "/home/agent/.codex" {
		t.Errorf("surviving mount target = %q, want /home/agent/.codex", got[0].Target)
	}

	// config.toml must now live inside the directory mount's source so the
	// single directory mount exposes it at /home/agent/.codex/config.toml.
	placed := filepath.Join(dirSource, "config.toml")
	data, err := os.ReadFile(placed)
	if err != nil {
		t.Fatalf("config.toml not relocated into dir mount source: %v", err)
	}
	if string(data) != string(cfgBody) {
		t.Errorf("relocated config.toml body mismatch:\nwant %q\ngot  %q", cfgBody, data)
	}
}

// TestCollapseNestedMCPMounts_PreservesNonNestedMount confirms an MCP
// mount that does not collide with any directory mount is appended
// unchanged — the behavior every non-Codex agent relies on.
func TestCollapseNestedMCPMounts_PreservesNonNestedMount(t *testing.T) {
	cfgSource := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(cfgSource, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	existing := []sandboxcontainer.Volume{
		{Source: t.TempDir(), Target: "/home/agent/.claude", ReadOnly: false},
	}
	mcp := []MCPMount{
		{Source: cfgSource, Target: "/home/agent/.config/opencode/opencode.json", ReadOnly: true},
	}

	got, err := collapseNestedMCPMounts(existing, mcp)
	if err != nil {
		t.Fatalf("collapseNestedMCPMounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 mounts (non-nested preserved), got %d: %+v", len(got), got)
	}
	if got[1].Target != "/home/agent/.config/opencode/opencode.json" {
		t.Errorf("non-nested mount target = %q, want preserved", got[1].Target)
	}
	if !got[1].ReadOnly {
		t.Errorf("non-nested mount ReadOnly = false, want preserved true")
	}
}

// TestCollapseNestedMCPMounts_MissingSourceErrors confirms a relocation
// surfaces an error when the rendered MCP config file is missing rather
// than silently dropping it — a launch must fail loudly if the agent's
// ConfigureMCP promised a file that is not on disk.
func TestCollapseNestedMCPMounts_MissingSourceErrors(t *testing.T) {
	existing := []sandboxcontainer.Volume{
		{Source: t.TempDir(), Target: "/home/agent/.codex", ReadOnly: false},
	}
	mcp := []MCPMount{
		{Source: filepath.Join(t.TempDir(), "does-not-exist.toml"), Target: "/home/agent/.codex/config.toml", ReadOnly: true},
	}
	if _, err := collapseNestedMCPMounts(existing, mcp); err == nil {
		t.Fatal("expected error for missing MCP config source, got nil")
	}
}

// TestEnclosingDirMount_IdentityTargetNotEnclosing confirms a mount whose
// target exactly equals another mount's target is not treated as nested
// (only a strict parent encloses), so an identical-target MCP mount is
// appended rather than relocated.
func TestEnclosingDirMount_IdentityTargetNotEnclosing(t *testing.T) {
	mounts := []sandboxcontainer.Volume{
		{Source: "/h/a", Target: "/home/agent/.codex"},
	}
	if got := enclosingDirMount(mounts, "/home/agent/.codex"); got != nil {
		t.Fatalf("enclosingDirMount returned %+v for identical target, want nil", got)
	}
	if got := enclosingDirMount(mounts, "/home/agent/.codex/config.toml"); got == nil {
		t.Fatal("enclosingDirMount returned nil for nested target, want the dir mount")
	}
}
