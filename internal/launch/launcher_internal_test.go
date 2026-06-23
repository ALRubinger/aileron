package launch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
)

type emptyBinaryAgent struct{}

func (emptyBinaryAgent) Name() string           { return "empty" }
func (emptyBinaryAgent) BinaryNames() []string  { return nil }
func (emptyBinaryAgent) Args(_ Mode) []string   { return nil }
func (emptyBinaryAgent) Env() map[string]string { return nil }
func (emptyBinaryAgent) LLMEndpointEnv() string { return "" }
func (emptyBinaryAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (emptyBinaryAgent) AuthSpec() AuthSpec { return AuthSpec{} }

type namedBinaryAgent struct{ name string }

func (a namedBinaryAgent) Name() string           { return "named" }
func (a namedBinaryAgent) BinaryNames() []string  { return []string{a.name} }
func (a namedBinaryAgent) Args(_ Mode) []string   { return nil }
func (a namedBinaryAgent) Env() map[string]string { return nil }
func (a namedBinaryAgent) LLMEndpointEnv() string { return "" }
func (a namedBinaryAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (a namedBinaryAgent) AuthSpec() AuthSpec { return AuthSpec{} }

// publishedNameAgent reports its name as a published agent id (e.g. "claude")
// so Discover/EnrichValidateError treat it as having a published per-agent
// image. namedBinaryAgent hardcodes Name() to "named", which is unpublished.
type publishedNameAgent struct{ name string }

func (a publishedNameAgent) Name() string           { return a.name }
func (a publishedNameAgent) BinaryNames() []string  { return []string{a.name} }
func (a publishedNameAgent) Args(_ Mode) []string   { return nil }
func (a publishedNameAgent) Env() map[string]string { return nil }
func (a publishedNameAgent) LLMEndpointEnv() string { return "" }
func (a publishedNameAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (a publishedNameAgent) AuthSpec() AuthSpec { return AuthSpec{} }

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

// TestHostMCPEnvIncludesTokenWhenAdvertised is the regression test for
// issue #1406: host-launch aileron-mcp must receive AILERON_TOKEN so its
// /v1/actions discovery call carries Authorization: Bearer. Without it
// Codex's MCP server (which only sees the declared config env) 401s and
// falls back to the built-in tools.
func TestHostMCPEnvIncludesTokenWhenAdvertised(t *testing.T) {
	const (
		daemonURL = "http://127.0.0.1:48123"
		sessionID = "sess-1406"
		token     = "tok-abc123"
	)
	env := hostMCPEnv(daemonURL, sessionID, token)

	if got := env["AILERON_TOKEN"]; got != token {
		t.Fatalf("hostMCPEnv AILERON_TOKEN = %q, want %q", got, token)
	}
	wantBase := map[string]string{
		"AILERON_URL":          daemonURL,
		"AILERON_COMMS_URL":    daemonURL,
		"AILERON_SESSION_ID":   sessionID,
		"AILERON_APPROVAL_URL": daemonURL + "/approvals",
	}
	for k, want := range wantBase {
		if got := env[k]; got != want {
			t.Fatalf("hostMCPEnv[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestHostMCPEnvOmitsEmptyToken asserts that an unadvertised (empty) token
// is omitted entirely rather than written as AILERON_TOKEN="". An empty
// Bearer deterministically 401s, so the key must be absent so aileron-mcp
// treats it as "no token" rather than "malformed token".
func TestHostMCPEnvOmitsEmptyToken(t *testing.T) {
	env := hostMCPEnv("http://127.0.0.1:48123", "sess-1406", "")
	if _, present := env["AILERON_TOKEN"]; present {
		t.Fatalf("hostMCPEnv set AILERON_TOKEN with an empty token; want key absent")
	}
}

func TestLaunchSandboxRejectsAgentWithoutBinary(t *testing.T) {
	_, err := launchSandbox(nil, SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: emptyBinaryAgent{},
	}, nil, "aileron-sbx-test", func() {}, nil, nil)
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

// TestValidateSandboxEnrichesDevcontainerMismatch covers the wiring of the
// CWD-determined-tier enrichment into the launch validate path. When the
// underlying validate reports the agent CLI is missing on a devcontainer-tier
// plan for a published agent, validateSandbox must surface the enriched message
// naming the tier, the discovered devcontainer path, and the published image.
func TestValidateSandboxEnrichesDevcontainerMismatch(t *testing.T) {
	orig := validateSandboxImageForLaunch
	t.Cleanup(func() { validateSandboxImageForLaunch = orig })
	validateSandboxImageForLaunch = func(_ context.Context, _ SandboxLaunchPlan, _ LaunchConfig, _ map[string]string, _ []sandboxcontainer.Volume, _ string) error {
		return errors.New("validate sandbox image image:test: agent command not found in sandbox image: claude: exit status 127")
	}

	workDir := t.TempDir()
	err := validateSandbox(context.Background(), SandboxLaunchPlan{
		Runtime: "docker",
		Image:   "image:test",
		Tier:    sandboxcomposition.TierDevcontainer,
	}, LaunchConfig{
		Agent: publishedNameAgent{name: "claude"},
		Dir:   workDir,
	}, nil)
	if err == nil {
		t.Fatal("validateSandbox: want error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		string(sandboxcomposition.TierDevcontainer),
		filepath.Join(workDir, sandboxcomposition.DefaultDevcontainerPath),
		sandboxcomposition.PublishedAgentImage("claude", version.Version),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("validateSandbox error missing %q: %s", want, msg)
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
	}, map[string]string{}, "aileron-sbx-test", func() {}, nil, logger)
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
	want := wantResolvedPath(t, linuxBin)
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
	want := wantResolvedPath(t, hostBin)
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

// TestSiblingCandidates_WindowsTriesExeFirst is the regression test for
// #1387: on Windows the launcher must probe `aileron-mcp.exe` (the real
// filename the Windows build produces) before the bare `aileron-mcp`,
// otherwise resolveSibling's os.Stat misses the binary, falls through,
// and the launcher writes an unresolvable `command = "aileron-mcp"` into
// Codex's config.toml — which fails at MCP startup with OS error 2.
//
// The suffix is a parameter so this Windows-specific ordering is
// verifiable from any host (CI runs these on Linux).
func TestSiblingCandidates_WindowsTriesExeFirst(t *testing.T) {
	got := siblingCandidates("aileron-mcp", ".exe")
	want := []string{"aileron-mcp.exe", "aileron-mcp"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("siblingCandidates(\"aileron-mcp\", \".exe\") = %v, want %v", got, want)
	}
}

// TestSiblingCandidates_NonWindowsBareNameOnly confirms that off Windows
// (empty suffix) the lookup is unchanged: a single bare candidate, no
// spurious `.exe` probe.
func TestSiblingCandidates_NonWindowsBareNameOnly(t *testing.T) {
	got := siblingCandidates("aileron-mcp", "")
	if len(got) != 1 || got[0] != "aileron-mcp" {
		t.Fatalf("siblingCandidates(\"aileron-mcp\", \"\") = %v, want [aileron-mcp]", got)
	}
}

// TestSiblingCandidates_AlreadySuffixed guards against a double suffix
// when a caller passes a name that already ends in the platform suffix:
// the bare name is returned as-is rather than producing `foo.exe.exe`.
func TestSiblingCandidates_AlreadySuffixed(t *testing.T) {
	got := siblingCandidates("aileron-mcp.exe", ".exe")
	if len(got) != 1 || got[0] != "aileron-mcp.exe" {
		t.Fatalf("siblingCandidates(\"aileron-mcp.exe\", \".exe\") = %v, want [aileron-mcp.exe]", got)
	}
}

// TestArchSuffixedCandidates_PerPlatform locks the exact ordered candidate
// filenames archSuffixedCandidates produces for every supported target
// platform. The platform axis (goos/goarch) is a parameter, so all M
// targets are exercised from a single host (CI runs on one OS): the
// windows `.exe` suffix and the darwin/linux ordering are each verified
// here without needing to build on those platforms.
func TestArchSuffixedCandidates_PerPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         []string
	}{
		{"darwin", "arm64", []string{"aileron-mcp-darwin-arm64", "aileron-mcp"}},
		{"darwin", "amd64", []string{"aileron-mcp-darwin-amd64", "aileron-mcp"}},
		{"windows", "amd64", []string{"aileron-mcp-windows-amd64.exe", "aileron-mcp"}},
		{"linux", "amd64", []string{"aileron-mcp-linux-amd64", "aileron-mcp"}},
		{"linux", "arm64", []string{"aileron-mcp-linux-arm64", "aileron-mcp"}},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			got := archSuffixedCandidates("aileron-mcp", tc.goos, tc.goarch)
			if len(got) != len(tc.want) || got[0] != tc.want[0] || got[1] != tc.want[1] {
				t.Fatalf("archSuffixedCandidates(%q, %q, %q) = %v, want %v",
					"aileron-mcp", tc.goos, tc.goarch, got, tc.want)
			}
		})
	}
}

// TestArchSuffixedCandidates_AlreadySuffixed guards against a double `.exe`
// when a windows-target candidate name already carries the platform
// extension: the suffixed candidate is returned as-is rather than producing
// `...amd64.exe.exe`. Mirrors TestSiblingCandidates_AlreadySuffixed.
func TestArchSuffixedCandidates_AlreadySuffixed(t *testing.T) {
	// Pass a name that, once platform-suffixed, already ends in `.exe`.
	got := archSuffixedCandidates("aileron-mcp-windows-amd64.exe", "windows", "amd64")
	want := []string{"aileron-mcp-windows-amd64.exe-windows-amd64.exe", "aileron-mcp-windows-amd64.exe"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("archSuffixedCandidates double-suffix guard = %v, want %v", got, want)
	}
}

// TestGoosExeSuffix confirms the target-GOOS exe extension is applied per
// the *target* platform, not the host: windows yields `.exe` and every
// other GOOS yields empty, verifiable from any build host.
func TestGoosExeSuffix(t *testing.T) {
	cases := map[string]string{
		"windows": ".exe",
		"linux":   "",
		"darwin":  "",
	}
	for goos, want := range cases {
		if got := goosExeSuffix(goos); got != want {
			t.Fatalf("goosExeSuffix(%q) = %q, want %q", goos, got, want)
		}
	}
}

// TestResolveManagedBinary_LocatesNonHostTargetSibling exercises the
// generalized selection unit for a *non-host* target: a
// `<name>-<goos>-<goarch>` sibling placed next to the running binary
// resolves to its absolute path even though goos/goarch differ from the
// host. This is the M-way generalization the issue mandates — selecting a
// managed binary for any platform from one host.
func TestResolveManagedBinary_LocatesNonHostTargetSibling(t *testing.T) {
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Target a platform deliberately different from the host so the test
	// proves the axis is a parameter, not runtime.GOOS/GOARCH.
	goos, goarch := "linux", "amd64"
	managed := filepath.Join(dir, "managed-tool-"+goos+"-"+goarch)
	if err := os.WriteFile(managed, []byte("the managed binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveManagedBinary(selfPath, "managed-tool", goos, goarch)
	if err != nil {
		t.Fatalf("resolveManagedBinary: %v", err)
	}
	want := wantResolvedPath(t, managed)
	if got != want {
		t.Fatalf("resolveManagedBinary = %q, want %q (the per-platform sibling)", got, want)
	}
}

// TestResolveManagedBinary_UnwrapsScoopShimOnWindowsTarget proves the
// Windows reparse/Scoop-shim tail is preserved for the generalized lookup:
// a windows/amd64 managed binary published as `<name>-windows-amd64.exe`
// but installed through a Scoop `.shim` + `current` junction must resolve
// to the junction-free versioned target, exactly like resolveSibling does
// for aileron-mcp. A POSIX symlink stands in for the Windows junction so
// this is verifiable on any host.
func TestResolveManagedBinary_UnwrapsScoopShimOnWindowsTarget(t *testing.T) {
	// Canonicalize the base so the only reparse point under test is the
	// `current` link, not t.TempDir()'s own /var -> /private/var on macOS.
	dir := wantResolvedPath(t, t.TempDir())
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The published windows candidate name is `<name>-windows-amd64.exe`;
	// the Scoop stub sits next to aileron under that name.
	candidate := "managed-tool-windows-amd64.exe"
	stub := filepath.Join(dir, candidate)
	if err := os.WriteFile(stub, []byte("scoop shim stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	versionedDir := filepath.Join(dir, "apps", "managed-tool", "0.0.9")
	if err := os.MkdirAll(versionedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(versionedDir, candidate)
	if err := os.WriteFile(realBin, []byte("the real managed binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	currentLink := filepath.Join(dir, "apps", "managed-tool", "current")
	if err := os.Symlink(versionedDir, currentLink); err != nil {
		t.Skipf("cannot create symlink on this host (unprivileged Windows?): %v", err)
	}
	shimTarget := filepath.Join(currentLink, candidate)
	sidecar := filepath.Join(dir, "managed-tool-windows-amd64.shim")
	if err := os.WriteFile(sidecar, []byte("path = \""+shimTarget+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveManagedBinary(selfPath, "managed-tool", "windows", "amd64")
	if err != nil {
		t.Fatalf("resolveManagedBinary: %v", err)
	}
	if got != realBin {
		t.Fatalf("resolveManagedBinary = %q, want junction-free %q (Scoop shim + current junction unwrapped)", got, realBin)
	}
}

// TestResolveManagedBinary_FallsBackToBareName confirms the bare-name
// fallback candidate is honored: when no `<name>-<goos>-<goarch>` sibling
// exists but a manually placed extension-less binary does, the bare name
// resolves (same fallback contract as siblingCandidates).
func TestResolveManagedBinary_FallsBackToBareName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bare-named sibling is not the Windows on-disk shape; covered by the shim/candidate tests")
	}
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(dir, "managed-tool")
	if err := os.WriteFile(bare, []byte("manually placed binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	got, err := resolveManagedBinary(selfPath, "managed-tool", "linux", "amd64")
	if err != nil {
		t.Fatalf("resolveManagedBinary: %v", err)
	}
	want := wantResolvedPath(t, bare)
	if got != want {
		t.Fatalf("resolveManagedBinary = %q, want bare-name fallback %q", got, want)
	}
}

// TestResolveManagedBinary_PropagatesMissingError confirms a real error is
// returned (not an empty path) when neither the platform-suffixed nor the
// bare candidate resolves.
func TestResolveManagedBinary_PropagatesMissingError(t *testing.T) {
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	if _, err := resolveManagedBinary(selfPath, "managed-tool", "linux", "amd64"); err == nil {
		t.Fatal("expected error when no candidate resolves")
	}
}

// TestResolveSibling_FindsHostSibling exercises resolveSibling end-to-end
// on the current host: a bare-named sibling placed next to the running
// binary resolves to its absolute path. This locks the non-Windows
// happy path so the #1387 candidate-ordering change does not regress the
// common Linux/macOS install layout.
func TestResolveSibling_FindsHostSibling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bare-named sibling is not the Windows on-disk shape; covered by siblingCandidates tests")
	}
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(dir, "aileron-mcp")
	if err := os.WriteFile(mcp, []byte("mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSibling(selfPath, "aileron-mcp")
	if err != nil {
		t.Fatalf("resolveSibling: %v", err)
	}
	want := wantResolvedPath(t, mcp)
	if got != want {
		t.Fatalf("resolveSibling = %q, want %q", got, want)
	}
}

// TestResolveSibling_UnwrapsScoopShim is the regression test for #1405:
// on a Scoop install the sibling next to `aileron` is the ~133 KB Scoop
// launcher stub, not the real binary, and an agent that spawns it under a
// Windows sandbox (Codex) fails with OS error 2. resolveSibling must read
// the co-located `<name>.shim` sidecar and write the real target into the
// agent's MCP config instead of the stub path. The lookup is keyed off the
// sidecar (not GOOS) so it is verifiable from any host.
func TestResolveSibling_UnwrapsScoopShim(t *testing.T) {
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron"+exeSuffix())
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The Scoop shim stub sits next to aileron; the real binary lives under
	// apps/.../current. parseShimPath unwraps the stub to the real target.
	stub := filepath.Join(dir, "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(stub, []byte("scoop shim stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(dir, "apps", "aileron", "current")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(realDir, "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(realBin, []byte("the real 14 MB binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "aileron-mcp.shim")
	if err := os.WriteFile(sidecar, []byte("path = \""+realBin+"\"\nargs = \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSibling(selfPath, "aileron-mcp")
	if err != nil {
		t.Fatalf("resolveSibling: %v", err)
	}
	want := wantResolvedPath(t, realBin)
	if got != want {
		t.Fatalf("resolveSibling = %q, want the real target %q (not the Scoop shim stub)", got, want)
	}
}

// wantResolvedPath returns the absolute, reparse-point-resolved form of p,
// matching what resolveSibling now emits. It is needed because resolveSibling
// collapses symlinks/junctions (resolveReparsePoints) and t.TempDir() is
// itself a symlink on macOS (/var -> /private/var); a bare filepath.Abs
// would otherwise mismatch the resolved result on that host.
func wantResolvedPath(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", p, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%q): %v", abs, err)
	}
	return resolved
}

// TestResolveSibling_ResolvesScoopCurrentJunction is the regression test for
// the successor to #1405: unwrapping the Scoop stub to the real target was
// necessary but not sufficient, because that target rides the
// `...\apps\<app>\current\...` directory junction. A normal shell launch
// traverses the junction, but Codex's elevated Windows sandbox cannot spawn a
// command whose path crosses it and still fails with OS error 2.
// resolveSibling must collapse the junction to the versioned install dir
// (`...\apps\<app>\0.0.9\...`) before the command is written into config.toml.
// A POSIX symlink stands in for the Windows junction so this is verifiable on
// any host.
func TestResolveSibling_ResolvesScoopCurrentJunction(t *testing.T) {
	// Canonicalize the base so the only reparse point under test is the
	// `current` link, not t.TempDir()'s own /var -> /private/var on macOS.
	dir := wantResolvedPath(t, t.TempDir())
	selfPath := filepath.Join(dir, "aileron"+exeSuffix())
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(stub, []byte("scoop shim stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	versionedDir := filepath.Join(dir, "apps", "aileron", "0.0.9")
	if err := os.MkdirAll(versionedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(versionedDir, "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(realBin, []byte("the real 14 MB binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// `current` is a junction to the versioned dir on a real Scoop install;
	// a directory symlink is the cross-platform stand-in.
	currentLink := filepath.Join(dir, "apps", "aileron", "current")
	if err := os.Symlink(versionedDir, currentLink); err != nil {
		t.Skipf("cannot create symlink on this host (unprivileged Windows?): %v", err)
	}
	// The Scoop sidecar points at the binary *through* the current junction,
	// exactly as resolveShimTarget would hand it back.
	shimTarget := filepath.Join(currentLink, "aileron-mcp"+exeSuffix())
	sidecar := filepath.Join(dir, "aileron-mcp.shim")
	if err := os.WriteFile(sidecar, []byte("path = \""+shimTarget+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSibling(selfPath, "aileron-mcp")
	if err != nil {
		t.Fatalf("resolveSibling: %v", err)
	}
	if got != realBin {
		t.Fatalf("resolveSibling = %q, want junction-free %q (sandbox spawns the versioned path, not the `current` junction)", got, realBin)
	}
}

// TestResolveSibling_NoShimSidecarReturnsSiblingUnchanged confirms the
// common (non-Scoop) layout is untouched: with no `<name>.shim` sidecar,
// resolveSibling returns the sibling itself, not an empty or altered path.
func TestResolveSibling_NoShimSidecarReturnsSiblingUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bare-named sibling is not the Windows on-disk shape; covered by siblingCandidates tests")
	}
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "aileron")
	if err := os.WriteFile(selfPath, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(dir, "aileron-mcp")
	if err := os.WriteFile(mcp, []byte("real binary, no shim"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSibling(selfPath, "aileron-mcp")
	if err != nil {
		t.Fatalf("resolveSibling: %v", err)
	}
	want := wantResolvedPath(t, mcp)
	if got != want {
		t.Fatalf("resolveSibling = %q, want %q (sibling unchanged when no shim sidecar)", got, want)
	}
}

// TestResolveShimTarget_MissingTargetFallsBackToShim guards the
// best-effort contract: a `.shim` sidecar that names a target which does
// not exist on disk (stale/broken shim) must leave the original path
// unchanged rather than returning a dangling path the agent cannot spawn.
func TestResolveShimTarget_MissingTargetFallsBackToShim(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(stub, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "aileron-mcp.shim")
	missing := filepath.Join(dir, "does-not-exist", "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(sidecar, []byte("path = \""+missing+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveShimTarget(stub); got != stub {
		t.Fatalf("resolveShimTarget = %q, want original stub %q when target is missing", got, stub)
	}
}

// TestResolveShimTarget_NoPathKeyReturnsBinUnchanged covers a sidecar
// that exists but declares no `path` key: parseShimPath yields "" and the
// original binary path must be returned, never an empty string.
func TestResolveShimTarget_NoPathKeyReturnsBinUnchanged(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(stub, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "aileron-mcp.shim")
	if err := os.WriteFile(sidecar, []byte("args = --foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveShimTarget(stub); got != stub {
		t.Fatalf("resolveShimTarget = %q, want original stub %q when sidecar has no path key", got, stub)
	}
}

// TestResolveShimTarget_RelativeTargetResolvesAgainstBinDir covers the
// defensive normalization for a sidecar whose `path` is relative: it is
// resolved against the stub's directory before the existence check, and
// the absolute real target is returned.
func TestResolveShimTarget_RelativeTargetResolvesAgainstBinDir(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "aileron-mcp"+exeSuffix())
	if err := os.WriteFile(stub, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	realName := "real-aileron-mcp" + exeSuffix()
	if err := os.WriteFile(filepath.Join(dir, realName), []byte("real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "aileron-mcp.shim")
	if err := os.WriteFile(sidecar, []byte("path = "+realName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolveShimTarget(stub)
	want, _ := filepath.Abs(filepath.Join(dir, realName))
	if got != want {
		t.Fatalf("resolveShimTarget = %q, want %q (relative target resolved against bin dir)", got, want)
	}
}

// TestParseShimPath covers the sidecar parser's contract directly: it
// returns the first `path` value with surrounding quotes stripped, and ""
// when no `path` key is present.
func TestParseShimPath(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string
	}{
		// A real Scoop .shim records the path verbatim with single
		// backslashes (it is not TOML, so there is no escaping to undo);
		// parseShimPath only strips the surrounding quotes.
		{"quoted windows path", `path = "C:\Users\alr\scoop\apps\aileron\current\aileron-mcp.exe"` + "\nargs = \n", `C:\Users\alr\scoop\apps\aileron\current\aileron-mcp.exe`},
		{"unquoted", "path = /usr/local/bin/real\n", "/usr/local/bin/real"},
		{"no path key", "args = --foo\nother = bar\n", ""},
		{"empty", "", ""},
		{"path among other keys", "args = x\npath = /real\n", "/real"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := parseShimPath(c.in); got != c.want {
				t.Fatalf("parseShimPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
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

// codexNestedMCPMount returns a Codex-shaped AuthSpec whose single
// FileBinding produces a directory mount at /home/agent/.codex/
// (MountAsFile=false), mirroring the real Codex AuthSpec the launcher
// materializes. With an empty vault the no-creds path runs ensureGroup
// and keeps the .codex dir mounted because MountAsFile is false, exactly
// the shape that triggered issue #1488.
func codexNestedMCPSpec() AuthSpec {
	return AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/codex/oauth",
			ContainerPath: "/home/agent/.codex/auth.json",
			Mode:          0o600,
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
}

// TestChownAfterMCPPlacement_CodexOrdering is the #1488 regression. The
// chown that re-owns the AuthSpec transient tree to the foreign agent UID
// (mode 0700) must run AFTER the launcher places Codex's config.toml into
// the .codex directory mount, otherwise the unprivileged host-side write
// in collapseNestedMCPMounts fails with EPERM ("permission denied").
//
// The test drives the production ordering: prepareAuthSpec renders the
// AuthSpec (deferring the chown into prep.ChownFn), collapseNestedMCPMounts
// places config.toml into the .codex dir mount source, THEN prep.ChownFn
// runs. The chown hook emulates the foreign-UID 0700 state by stripping
// write permission from the group dir; because the placement already ran,
// it succeeds. A control assertion shows the inverse ordering (chown then
// place) fails — proving the test would catch a regression that re-ordered
// the chown back before the placement.
func TestChownAfterMCPPlacement_CodexOrdering(t *testing.T) {
	daemon := newFakeDaemon() // empty vault: no-creds path, .codex still mounted

	// The chown hook emulates the EPERM the real chown-to-foreign-UID-0700
	// causes for an unprivileged host process: it removes write permission
	// from the .codex group dir so any later host-side write fails.
	var chowned bool
	hook := func(dir string) error {
		chowned = true
		codexDir := filepath.Join(dir, "home", "agent", ".codex")
		if err := os.Chmod(codexDir, 0o500); err != nil {
			return err
		}
		// Restore at cleanup so prep.Cleanup's RemoveAll can recurse.
		t.Cleanup(func() { _ = os.Chmod(codexDir, 0o700) })
		return nil
	}

	prep, err := prepareAuthSpec(context.Background(), "codex", codexNestedMCPSpec(),
		daemon, newTestLogger(), nil, hook, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// The chown must be deferred, not run at prep time.
	if chowned {
		t.Fatal("chown ran during prepareAuthSpec; #1488 requires it to be deferred so the MCP config is placed first")
	}
	if len(prep.Mounts) != 1 || prep.Mounts[0].Target != "/home/agent/.codex" {
		t.Fatalf("expected one /home/agent/.codex dir mount, got %+v", prep.Mounts)
	}

	// Build the sandbox mount list as launchSandbox does and run the
	// nested-collapse placement BEFORE the chown.
	cfgSource := filepath.Join(t.TempDir(), "config.toml")
	cfgBody := []byte("[mcp_servers.aileron]\ncommand = \"/usr/local/bin/aileron-mcp\"\n")
	if err := os.WriteFile(cfgSource, cfgBody, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}
	mcp := []MCPMount{{Source: cfgSource, Target: "/home/agent/.codex/config.toml", ReadOnly: true}}

	collapsed, err := collapseNestedMCPMounts(prep.Mounts, mcp)
	if err != nil {
		t.Fatalf("collapseNestedMCPMounts (placement must succeed before chown): %v", err)
	}
	// config.toml must now live inside the .codex dir mount source.
	placed := filepath.Join(prep.Mounts[0].Source, "config.toml")
	if data, err := os.ReadFile(placed); err != nil {
		t.Fatalf("config.toml not placed into .codex dir mount: %v", err)
	} else if string(data) != string(cfgBody) {
		t.Errorf("placed config.toml = %q, want %q", data, cfgBody)
	}
	if n := nestedMountTarget(collapsed); n != "" {
		t.Fatalf("nested bind mount survived collapse: %q", n)
	}

	// Now the chown runs (deferred). It must not error out of ChownFn.
	if err := prep.ChownFn(); err != nil {
		t.Fatalf("prep.ChownFn: %v", err)
	}
	if !chowned {
		t.Fatal("prep.ChownFn did not invoke the chown hook")
	}

	// Control: prove the bug the ordering prevents. Once the dir is
	// chowned (0500 here), a fresh placement into it fails with EPERM —
	// which is exactly what the old "chown during prep" ordering caused.
	cfg2 := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg2, cfgBody, 0o600); err != nil {
		t.Fatalf("seed second config.toml: %v", err)
	}
	if os.Geteuid() != 0 { // root bypasses DAC, so the control only holds unprivileged
		if _, err := collapseNestedMCPMounts(
			[]sandboxcontainer.Volume{{Source: prep.Mounts[0].Source, Target: "/home/agent/.codex"}},
			[]MCPMount{{Source: cfg2, Target: "/home/agent/.codex/sub/config.toml"}},
		); err == nil {
			t.Fatal("placement into a chowned (write-denied) dir unexpectedly succeeded; the control proves the ordering matters")
		}
	}
}
