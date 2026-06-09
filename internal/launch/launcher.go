package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	sandboxdiscovery "github.com/ALRubinger/aileron/internal/sandbox/discovery"
	"github.com/ALRubinger/aileron/internal/version"
	"golang.org/x/term"
)

// MCPServerName is the name Aileron registers itself under in agents'
// MCP-server configs. The agent surfaces Aileron's tools as
// `mcp__<MCPServerName>__<tool-name>`. Agent definitions reference this
// when they wire host-level allowlists (e.g. Claude Code's
// `--allowedTools mcp__aileron`) so the host CLI does not double-prompt
// for tools the daemon already gates (ADR-0009 / ADR-0010).
const MCPServerName = "aileron"

const (
	sandboxToolsFilePath = "/etc/aileron/tools.txt"
	sandboxShimsDirPath  = "/usr/local/bin"
	sandboxProxyCAPath   = "/etc/aileron/proxy/ca.pem"
	// sandboxMCPBinPath is where the launcher bind-mounts the host-built
	// aileron-mcp binary inside the container. The agent's MCP client
	// execs this path as its stdio child. See ADR-0024.
	sandboxMCPBinPath = "/usr/local/bin/aileron-mcp"
	// sandboxMCPBinName is the basename of the in-container MCP binary;
	// reserved so no future shim mount can collide with it.
	sandboxMCPBinName = "aileron-mcp"
)

// LaunchConfig holds the configuration for launching an agent.
type LaunchConfig struct {
	// Agent is the agent to launch.
	Agent Agent
	// Args are extra arguments to pass to the agent binary.
	Args []string
	// Dir is the working directory for the agent. If empty, the current
	// directory is used.
	Dir string
	// LogLevel sets the session log verbosity (e.g. slog.LevelDebug).
	LogLevel slog.Level
	// SandboxRuntime selects the container runtime used to run the
	// agent in the prepared sandbox image. Empty or "off" preserves
	// today's direct host launch path. "auto", "docker", and "podman"
	// prepare the composition-selected image and execute the agent in a
	// one-shot container.
	SandboxRuntime string
	// SandboxBuildPolicy controls launch-time builds for buildable
	// sandbox tiers. Empty defaults to auto for launch.
	SandboxBuildPolicy string
}

// LaunchResult holds the outcome of a launched agent process.
type LaunchResult struct {
	ExitCode int
}

// SandboxLaunchPlan is the sandbox image selected or prepared for a launch.
// Runtime injection, watcher refresh, and shell interception are follow-on
// launch work.
type SandboxLaunchPlan struct {
	Runtime string
	Image   string
	Tier    sandboxcomposition.Tier
	Built   bool
}

// ResolveBinary searches PATH for the first matching binary name from
// the given candidates. Returns the absolute path or an error.
func ResolveBinary(names []string) (string, error) {
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("could not find any of %v on PATH", names)
}

// resolveSibling looks for a binary next to the given executable path,
// then falls back to searching PATH.
func resolveSibling(selfPath, name string) (string, error) {
	dir := filepath.Dir(selfPath)
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err == nil {
		return filepath.Abs(candidate)
	}
	return exec.LookPath(name)
}

// resolveMCPBinary locates aileron-mcp. The MCP server is a required
// runtime dependency of `aileron launch` per ADR-0015 — without it the
// agent has no path to Aileron's tools, the vault, or the action-
// approval surface. Missing aileron-mcp is a hard error with a
// remediation hint, not a silent skip.
//
// Lookup order: next to the running aileron binary first (the shape
// the Homebrew cask, deb/rpm/apk packages, and `task build:cli + task
// build:mcp` all produce), then $PATH (covers `go install` users who
// installed only the CLI or unusual layouts).
func resolveMCPBinary(selfPath string) (string, error) {
	mcpBin, err := resolveSibling(selfPath, "aileron-mcp")
	if err == nil {
		return mcpBin, nil
	}
	siblingDir := "<unknown>"
	if selfPath != "" {
		siblingDir = filepath.Dir(selfPath)
	}
	return "", fmt.Errorf(
		"aileron-mcp not found next to %s/aileron or on PATH; reinstall aileron from the official packages, or build and place aileron-mcp alongside aileron (task build:mcp; cp build/aileron-mcp %s/)",
		siblingDir, siblingDir,
	)
}

func sandboxLaunchEnabled(runtimeName string) bool {
	switch runtimeName {
	case "", "off":
		return false
	default:
		return true
	}
}

func validateSandboxRuntime(runtimeName string) error {
	switch runtimeName {
	case "", "off", sandboxcontainer.DefaultRuntime, "docker", "podman":
		return nil
	default:
		return fmt.Errorf("unsupported sandbox runtime %q (want off, auto, docker, or podman)", runtimeName)
	}
}

func normalizeLaunchBuildPolicy(policy string) (string, error) {
	normalized := strings.TrimSpace(policy)
	switch normalized {
	case "":
		return sandboxcontainer.BuildPolicyAuto, nil
	case sandboxcontainer.BuildPolicyAuto, sandboxcontainer.BuildPolicyAlways, sandboxcontainer.BuildPolicyNever:
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported sandbox build policy %q (want auto, always, or never)", normalized)
	}
}

func prepareSandbox(ctx context.Context, workDir, runtimeName, buildPolicy string, stdout, stderr io.Writer) (SandboxLaunchPlan, error) {
	if err := validateSandboxRuntime(runtimeName); err != nil {
		return SandboxLaunchPlan{}, err
	}
	policy, err := normalizeLaunchBuildPolicy(buildPolicy)
	if err != nil {
		return SandboxLaunchPlan{}, err
	}
	plan, err := sandboxcomposition.Discover(workDir, version.Version)
	if err != nil {
		return SandboxLaunchPlan{}, err
	}
	result, err := sandboxcontainer.Builder{
		Runtime: runtimeName,
		Stdout:  stdout,
		Stderr:  stderr,
	}.Build(ctx, sandboxcontainer.BuildOptions{
		WorkDir: workDir,
		Plan:    plan,
		Policy:  policy,
	})
	if errors.Is(err, sandboxcontainer.ErrNoBuildRequired) {
		runtime, runtimeErr := sandboxcontainer.ResolveRuntime(runtimeName)
		if runtimeErr != nil {
			return SandboxLaunchPlan{}, runtimeErr
		}
		return SandboxLaunchPlan{
			Runtime: runtime,
			Image:   result.Image,
			Tier:    result.Tier,
		}, nil
	}
	if err != nil {
		return SandboxLaunchPlan{}, err
	}
	return SandboxLaunchPlan{
		Runtime: result.Runtime,
		Image:   result.Image,
		Tier:    result.Tier,
		Built:   result.Built,
	}, nil
}

var prepareSandboxForLaunch = prepareSandbox

func validateSandbox(ctx context.Context, plan SandboxLaunchPlan, config LaunchConfig, agentEnv map[string]string, extraMounts ...sandboxcontainer.Volume) error {
	commandName := firstAgentBinary(config.Agent)
	if commandName == "" {
		return fmt.Errorf("agent %q has no container command", config.Agent.Name())
	}
	mounts, cleanupMounts, err := sandboxRuntimeMounts(commandName)
	if err != nil {
		return err
	}
	defer cleanupMounts()
	mounts = append(mounts, extraMounts...)
	if err := validateSandboxImageForLaunch(ctx, plan, config, agentEnv, mounts, commandName); err != nil {
		return fmt.Errorf("sandbox image %s is not launchable for %s: %w", plan.Image, config.Agent.Name(), err)
	}
	return nil
}

func validateSandboxImage(ctx context.Context, plan SandboxLaunchPlan, config LaunchConfig, agentEnv map[string]string, mounts []sandboxcontainer.Volume, commandName string) error {
	if err := (sandboxcontainer.Builder{
		Runtime: plan.Runtime,
		Stdout:  io.Discard,
	}).Validate(ctx, sandboxcontainer.ValidateOptions{
		Runtime:               plan.Runtime,
		Image:                 plan.Image,
		WorkDir:           config.Dir,
		Env:               agentEnv,
		Volumes:           mounts,
		Command:           []string{commandName},
		RequireProxyTrust: agentEnv["AILERON_SANDBOX_PROXY_MODE"] != "",
	}); err != nil {
		return err
	}
	return nil
}

var validateSandboxForLaunch = validateSandbox
var validateSandboxImageForLaunch = validateSandboxImage

// Launch starts the agent as a child process under Aileron's daemon.
//
// Per ADR-0015 the launcher is the daemon-connection + MCP-registration
// + gateway-routing step. It does not replace $SHELL, install wrapper
// scripts, or audit shell commands the agent runs locally.
//
// Per ADR-0012 launch is a thin client of the user-scoped local daemon.
// It resolves (and auto-spawns) the daemon URL, registers the session,
// asks the agent to wire up aileron-mcp (Claude/Pi pass `--mcp-config`;
// Codex/Goose/OpenCode write config files), points the agent's
// LLM-endpoint env at the daemon if applicable, then execs the agent.
func Launch(ctx context.Context, config LaunchConfig) (LaunchResult, error) {
	stateDir, err := resolveStateDir()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("state dir: %w", err)
	}
	opts := spawn.Options{StateDir: stateDir}
	if os.Getenv("AILERON_API_URL") == "" {
		binary, err := resolveDaemonBinary()
		if err != nil {
			return LaunchResult{}, fmt.Errorf("locate daemon binary: %w", err)
		}
		opts.Binary = binary
	}
	resolveCtx, cancelResolve := context.WithTimeout(ctx, 10*time.Second)
	daemonURL, err := spawn.Resolve(resolveCtx, opts)
	cancelResolve()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("daemon: %w", err)
	}
	daemonURL = trimTrailingSlash(daemonURL)
	var daemonToken string
	if info, err := discovery.Read(stateDir); err == nil && trimTrailingSlash(info.URL) == daemonURL {
		daemonToken = info.Token
	}
	if daemonToken == "" {
		daemonToken = os.Getenv("AILERON_TOKEN")
	}
	client := newDaemonClient(daemonURL, daemonToken)

	sandboxEnabled := sandboxLaunchEnabled(config.SandboxRuntime)
	var sandboxPlan SandboxLaunchPlan
	if sandboxEnabled {
		plan, err := prepareSandboxForLaunch(ctx, config.Dir, config.SandboxRuntime, config.SandboxBuildPolicy, os.Stdout, os.Stderr)
		if err != nil {
			return LaunchResult{}, fmt.Errorf("prepare sandbox: %w", err)
		}
		sandboxPlan = plan
		fmt.Fprintf(os.Stderr, "aileron: sandbox image %s ready (tier=%s", sandboxPlan.Image, sandboxPlan.Tier)
		if sandboxPlan.Runtime != "" {
			fmt.Fprintf(os.Stderr, ", runtime=%s", sandboxPlan.Runtime)
		}
		if sandboxPlan.Built {
			fmt.Fprint(os.Stderr, ", built=true")
		}
		fmt.Fprintln(os.Stderr, ")")
	}

	regCtx, cancelReg := context.WithTimeout(ctx, daemonHTTPTimeout)
	sessionID, err := client.RegisterSession(regCtx, config.Agent.Name(), config.Dir)
	cancelReg()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("register session: %w", err)
	}
	sessionClosed := false
	defer func() {
		if sessionClosed {
			return
		}
		endCtx, cancelEnd := context.WithTimeout(context.Background(), daemonHTTPTimeout)
		_ = client.EndSession(endCtx, sessionID, nil)
		cancelEnd()
	}()

	agentEndpointURL := daemonURL
	if sandboxEnabled {
		agentEndpointURL = containerURLForRuntime(daemonURL, sandboxPlan.Runtime)
	}
	agentEnv := composeAgentEnv(config.Agent.Env(), config.Agent.LLMEndpointEnv(), agentEndpointURL)
	var proxyBootstrap sandboxProxyBootstrap
	if sandboxPlan.Image != "" {
		agentEnv["AILERON_SANDBOX_IMAGE"] = sandboxPlan.Image
		agentEnv["AILERON_SANDBOX_TIER"] = string(sandboxPlan.Tier)
		if sandboxPlan.Runtime != "" {
			agentEnv["AILERON_SANDBOX_RUNTIME"] = sandboxPlan.Runtime
		}
	}
	if sandboxEnabled {
		agentEnv["AILERON_URL"] = agentEndpointURL
		agentEnv["AILERON_API_URL"] = daemonAPIBaseURL(agentEndpointURL)
		agentEnv["AILERON_COMMS_URL"] = agentEndpointURL
		agentEnv["AILERON_SESSION_ID"] = sessionID
		agentEnv["AILERON_APPROVAL_URL"] = agentEndpointURL + "/approvals"
		agentEnv["AILERON_TOOLS_FILE"] = sandboxToolsFilePath
		agentEnv["AILERON_SHIMS_DIR"] = sandboxShimsDirPath
		if daemonToken != "" {
			agentEnv["AILERON_TOKEN"] = daemonToken
		}
		proxyBootstrap, err = prepareSandboxProxyBootstrap(stateDir, sessionID, agentEndpointURL, daemonToken)
		if err != nil {
			return LaunchResult{}, fmt.Errorf("prepare sandbox proxy bootstrap: %w", err)
		}
		applySandboxProxyBootstrapEnv(agentEnv, proxyBootstrap)
	}
	if sandboxEnabled {
		if err := validateSandboxForLaunch(ctx, sandboxPlan, config, agentEnv, proxyBootstrap.Mounts...); err != nil {
			return LaunchResult{}, err
		}
	}

	sessionLog, closeSessionLog := openSessionLogger(config.Dir, config.LogLevel)
	defer closeSessionLog()
	fmt.Fprintf(os.Stderr, "aileron: session log → %s\n", SessionLogPath(config.Dir))

	sessionLog.Info("session started",
		"agent", config.Agent.Name(),
		"session_id", sessionID,
		"dir", config.Dir,
		"binary", firstAgentBinary(config.Agent),
		"daemon_url", daemonURL,
		"daemon_log", filepath.Join(stateDir, "daemon.log"),
		"sandbox_image", sandboxPlan.Image,
		"sandbox_tier", sandboxPlan.Tier,
		"sandbox_runtime", sandboxPlan.Runtime,
		"sandbox_proxy_mode", proxyBootstrap.Mode,
		"sandbox_proxy_url", proxyBootstrap.ProxyURL,
		"sandbox_proxy_ca_path", proxyBootstrap.CAPath,
	)

	probeCtx, cancelProbe := context.WithTimeout(ctx, daemonHTTPTimeout)
	locked, ok := client.LocalVaultLocked(probeCtx)
	cancelProbe()
	printStartupBanner(os.Stderr, daemonURL, sessionID, SessionLogPath(config.Dir), ok && locked)

	var result LaunchResult
	var runErr error
	if sandboxEnabled {
		result, runErr = launchSandbox(ctx, sandboxPlan, config, agentEnv, proxyBootstrap.Mounts...)
	} else {
		result, runErr = launchHost(ctx, config, daemonURL, sessionID, agentEnv)
	}

	endCtx, cancelEnd := context.WithTimeout(context.Background(), daemonHTTPTimeout)
	exit := result.ExitCode
	if endErr := client.EndSession(endCtx, sessionID, &exit); endErr != nil {
		sessionLog.Warn("end session", "error", endErr)
	}
	cancelEnd()
	sessionClosed = true

	sessionLog.Info("session ended", "exit_code", result.ExitCode)
	return result, runErr
}

// composeAgentEnv merges the agent's env map with the LLM-endpoint
// override (e.g. ANTHROPIC_BASE_URL → daemonURL). Empty endpointEnv or
// empty url skips the override entirely.
func composeAgentEnv(agentEnv map[string]string, endpointEnv, url string) map[string]string {
	merged := make(map[string]string, len(agentEnv)+1)
	for k, v := range agentEnv {
		merged[k] = v
	}
	if endpointEnv != "" && url != "" {
		merged[endpointEnv] = url
	}
	return merged
}

func firstAgentBinary(agent Agent) string {
	names := agent.BinaryNames()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func containerURLForRuntime(rawURL, runtimeName string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		host = parsed.Hostname()
		port = parsed.Port()
	}
	if !isLoopbackHost(host) {
		return rawURL
	}
	switch runtimeName {
	case "podman":
		host = "host.containers.internal"
	default:
		host = "host.docker.internal"
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else {
		parsed.Host = host
	}
	return parsed.String()
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func daemonAPIBaseURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/v1"
}

// resolveStateDir returns ~/.aileron, the state directory the daemon
// publishes its discovery files into.
func resolveStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aileron"), nil
}

// resolveDaemonBinary locates the daemon binary — a sibling of the
// running aileron binary named "aileron-server" — and falls back to
// PATH. The name matches the goreleaser/Homebrew artifact so installs
// from a packaged distribution work without further setup.
func resolveDaemonBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(filepath.Dir(self), "aileron-server")
	if _, err := os.Stat(candidate); err == nil {
		return filepath.Abs(candidate)
	}
	return exec.LookPath("aileron-server")
}

// printStartupBanner writes a single line on stderr before exec'ing the
// agent, naming the daemon URL and session id. When vaultLocked is
// true, a follow-up line points the user at the unlock surface.
func printStartupBanner(w io.Writer, daemonURL, sessionID, logPath string, vaultLocked bool) {
	if daemonURL == "" {
		fmt.Fprintf(w, "✈️  Aileron — session %s — log %s\n", sessionID, logPath)
		return
	}
	fmt.Fprintf(w, "✈️  Aileron — webapp %s — session %s — log %s\n", daemonURL, sessionID, logPath)
	if vaultLocked {
		fmt.Fprintf(w, "✈️  Vault locked — open %s and enter your passphrase to unlock.\n", daemonURL)
	}
}

func launchHost(ctx context.Context, config LaunchConfig, daemonURL, sessionID string, agentEnv map[string]string) (LaunchResult, error) {
	agentPath, err := ResolveBinary(config.Agent.BinaryNames())
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent %q: %w", config.Agent.Name(), err)
	}

	// Resolve aileron-mcp up front for the host launch path. Sandbox
	// launch does not revive aileron-mcp as the container runtime model;
	// container-side shims/proxy bootstrap land in later #796/#801 slices.
	selfPath, _ := os.Executable()
	mcpBin, err := resolveMCPBinary(selfPath)
	if err != nil {
		return LaunchResult{}, err
	}

	allArgs := append(config.Agent.Args(), config.Args...)
	mcpEnv := map[string]string{
		"AILERON_URL":          daemonURL,
		"AILERON_COMMS_URL":    daemonURL,
		"AILERON_SESSION_ID":   sessionID,
		"AILERON_APPROVAL_URL": daemonURL + "/approvals",
	}
	extraArgs, mcpErr := config.Agent.ConfigureMCP(mcpBin, mcpEnv, config.Dir)
	if mcpErr != nil {
		return LaunchResult{}, fmt.Errorf("configuring MCP for %s: %w", config.Agent.Name(), mcpErr)
	}
	allArgs = append(allArgs, extraArgs...)

	cmd := exec.CommandContext(ctx, agentPath, allArgs...)
	cmd.Env = buildEnv(agentEnv)
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}
	return launchDirect(cmd, config)
}

func launchSandbox(ctx context.Context, plan SandboxLaunchPlan, config LaunchConfig, agentEnv map[string]string, extraMounts ...sandboxcontainer.Volume) (LaunchResult, error) {
	commandName := firstAgentBinary(config.Agent)
	if commandName == "" {
		return LaunchResult{}, fmt.Errorf("agent %q has no container command", config.Agent.Name())
	}
	// Reserve both the agent's own binary name AND aileron-mcp so no
	// connector-spec shim can clobber either at the mount layer.
	mounts, cleanupMounts, err := sandboxRuntimeMounts(commandName, sandboxMCPBinName)
	if err != nil {
		return LaunchResult{}, err
	}
	defer cleanupMounts()
	mounts = append(mounts, extraMounts...)

	// Resolve aileron-mcp on the host and bind-mount it read-only at the
	// container's well-known MCP binary path. Matches the resolution
	// shape host launch uses and the host-mount pattern
	// sandboxDiscoveryMounts uses for tools.txt and shims (ADR-0024).
	selfPath, _ := os.Executable()
	mcpBinHost, err := resolveMCPBinary(selfPath)
	if err != nil {
		return LaunchResult{}, err
	}
	mounts = append(mounts, sandboxcontainer.Volume{
		Source:   mcpBinHost,
		Target:   sandboxMCPBinPath,
		ReadOnly: true,
	})

	// Build the MCP env the in-container aileron-mcp needs to reach the
	// daemon. Reads from agentEnv so the URL rewrite for the runtime
	// (host.docker.internal vs host.containers.internal) is honored.
	mcpEnv := sandboxMCPEnv(agentEnv)
	extraArgs, mcpErr := config.Agent.ConfigureMCP(sandboxMCPBinPath, mcpEnv, config.Dir)
	if mcpErr != nil {
		return LaunchResult{}, fmt.Errorf("configuring MCP for %s: %w", config.Agent.Name(), mcpErr)
	}

	command := append([]string{commandName}, config.Agent.Args()...)
	command = append(command, extraArgs...)
	command = append(command, config.Args...)
	user := ""
	if sandboxProxyBootstrapActive(agentEnv) {
		user = "root"
		command = append([]string{"aileron-run-with-proxy-ca"}, command...)
	}
	_, err = sandboxcontainer.Builder{
		Runtime: plan.Runtime,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}.Run(ctx, sandboxcontainer.RunOptions{
		Runtime: plan.Runtime,
		Image:   plan.Image,
		WorkDir: config.Dir,
		Env:     agentEnv,
		Volumes: mounts,
		Command: command,
		User:    user,
		TTY:     term.IsTerminal(int(os.Stdin.Fd())),
	})
	if err != nil {
		return exitResult(err)
	}
	return LaunchResult{ExitCode: 0}, nil
}

// sandboxMCPEnv builds the env block aileron-mcp reads when it runs as
// an in-container stdio subprocess of the agent. Sources values from
// agentEnv so the URL rewrite for the container runtime
// (host.docker.internal on Docker, host.containers.internal on Podman)
// is preserved and no second source-of-truth for the daemon URL is
// introduced. AILERON_TOKEN is the post-mount credential per ADR-0024.
func sandboxMCPEnv(agentEnv map[string]string) map[string]string {
	mcpEnv := map[string]string{
		"AILERON_URL":          agentEnv["AILERON_URL"],
		"AILERON_COMMS_URL":    agentEnv["AILERON_COMMS_URL"],
		"AILERON_SESSION_ID":   agentEnv["AILERON_SESSION_ID"],
		"AILERON_APPROVAL_URL": agentEnv["AILERON_APPROVAL_URL"],
	}
	if token := agentEnv["AILERON_TOKEN"]; token != "" {
		mcpEnv["AILERON_TOKEN"] = token
	}
	return mcpEnv
}

func sandboxProxyBootstrapActive(agentEnv map[string]string) bool {
	return strings.TrimSpace(agentEnv["AILERON_SANDBOX_PROXY_MODE"]) != ""
}

func sandboxRuntimeMounts(reservedNames ...string) ([]sandboxcontainer.Volume, func(), error) {
	candidates := []sandboxcontainer.Volume{
		{
			Source:   action.DefaultDir(),
			Target:   "/opt/aileron/manifests/actions",
			ReadOnly: true,
		},
		{
			Source:   filepath.Join(cstore.DefaultRoot(), "connectors"),
			Target:   "/opt/aileron/manifests/connectors",
			ReadOnly: true,
		},
	}
	mounts := make([]sandboxcontainer.Volume, 0, len(candidates))
	for _, candidate := range candidates {
		info, err := os.Stat(candidate.Source)
		if err != nil || !info.IsDir() {
			continue
		}
		mounts = append(mounts, candidate)
	}
	discoveryMounts, cleanup, err := sandboxDiscoveryMounts(reservedNames...)
	if err != nil {
		return nil, cleanup, err
	}
	mounts = append(mounts, discoveryMounts...)
	return mounts, cleanup, nil
}

func sandboxDiscoveryMounts(reservedNames ...string) ([]sandboxcontainer.Volume, func(), error) {
	cleanup := func() {}
	store := action.NewStore(action.DefaultDir())
	if _, err := store.Load(); err != nil {
		return nil, cleanup, nil
	}
	actions := store.List()
	specs, err := connectorspec.LoadInstalled(cstore.DefaultRoot())
	if err != nil {
		return nil, cleanup, fmt.Errorf("load sandbox connector specs: %w", err)
	}
	toolsText, shimScripts, err := sandboxDiscoveryArtifacts(actions, specs, reservedNames...)
	if err != nil {
		return nil, cleanup, err
	}
	if len(toolsText) == 0 && len(shimScripts) == 0 {
		return nil, cleanup, nil
	}

	dir, err := os.MkdirTemp("", "aileron-sandbox-discovery-*")
	if err != nil {
		return nil, cleanup, fmt.Errorf("create sandbox discovery tempdir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	mounts := []sandboxcontainer.Volume{}
	if len(toolsText) > 0 {
		path := filepath.Join(dir, "tools.txt")
		if err := os.WriteFile(path, toolsText, 0o644); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("write sandbox tools manifest: %w", err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("chmod sandbox tools manifest: %w", err)
		}
		mounts = append(mounts, sandboxcontainer.Volume{
			Source:   path,
			Target:   sandboxToolsFilePath,
			ReadOnly: true,
		})
	}

	names := make([]string, 0, len(shimScripts))
	for name := range shimScripts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if isReservedSandboxCommand(name, reservedNames) {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, shimScripts[name], 0o755); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("write sandbox shim %s: %w", name, err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("chmod sandbox shim %s: %w", name, err)
		}
		mounts = append(mounts, sandboxcontainer.Volume{
			Source:   path,
			Target:   sandboxShimsDirPath + "/" + name,
			ReadOnly: true,
		})
	}
	return mounts, cleanup, nil
}

func sandboxDiscoveryArtifacts(actions []action.LoadedAction, specs []connectorspec.Spec, reservedNames ...string) ([]byte, map[string][]byte, error) {
	var toolsText []byte
	if actionTools := sandboxdiscovery.ToolsText(actions); len(actionTools) > 0 {
		toolsText = append(toolsText, actionTools...)
	}
	specTools, err := sandboxdiscovery.SpecToolsText(specs)
	if err != nil {
		return nil, nil, fmt.Errorf("render sandbox connector tools: %w", err)
	}
	if len(specTools) > 0 {
		toolsText = append(toolsText, specTools...)
	}

	shimScripts := sandboxdiscovery.ShimScripts(actions)
	if shimScripts == nil {
		shimScripts = map[string][]byte{}
	}
	specShims, err := sandboxdiscovery.SpecShimScripts(specs)
	if err != nil {
		return nil, nil, fmt.Errorf("render sandbox connector shims: %w", err)
	}
	for name, script := range specShims {
		if isReservedSandboxCommand(name, reservedNames) {
			return nil, nil, fmt.Errorf("sandbox connector shim %q conflicts with the selected agent command", name)
		}
		if _, ok := shimScripts[name]; ok {
			return nil, nil, fmt.Errorf("sandbox connector shim %q conflicts with an installed action shim", name)
		}
		shimScripts[name] = script
	}
	return toolsText, shimScripts, nil
}

func isReservedSandboxCommand(name string, reservedNames []string) bool {
	for _, reserved := range reservedNames {
		if name == reserved {
			return true
		}
	}
	return false
}

// launchDirect runs the agent with direct stdin/stdout/stderr passthrough.
func launchDirect(cmd *exec.Cmd, config LaunchConfig) (LaunchResult, error) {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return LaunchResult{}, fmt.Errorf("failed to start %s: %w", config.Agent.Name(), err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			_ = cmd.Process.Signal(sig)
		}
	}()

	err := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	return exitResult(err)
}

// exitResult extracts an exit code from a cmd.Wait error.
func exitResult(err error) (LaunchResult, error) {
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return LaunchResult{ExitCode: exitErr.ExitCode()}, nil
		}
		return LaunchResult{}, err
	}
	return LaunchResult{ExitCode: 0}, nil
}

// buildEnv creates the environment for the child process: the parent
// environment plus the agent's extra env vars. Per ADR-0015 no shim env
// vars (SHELL, AILERON_AGENT, etc.) are injected.
func buildEnv(agentEnv map[string]string) []string {
	managed := make(map[string]bool, len(agentEnv))
	for k := range agentEnv {
		managed[k] = true
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env)+len(agentEnv))
	for _, e := range env {
		eqIdx := indexByte(e, '=')
		if eqIdx < 0 {
			filtered = append(filtered, e)
			continue
		}
		if managed[e[:eqIdx]] {
			continue
		}
		filtered = append(filtered, e)
	}
	for k, v := range agentEnv {
		filtered = append(filtered, k+"="+v)
	}
	return filtered
}

// indexByte is strings.IndexByte without the import.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// SessionLogPath returns `<dir>/.aileron/session.log`. The session log
// captures the launched agent's stdio and is per-project. Distinct from
// the daemon's user-scoped audit store (ADR-0010), which records
// actions Aileron executes — not the agent's local stdio.
//
// Exported so `aileron sessions watch <id>` can resolve the same path
// the launcher writes to.
func SessionLogPath(dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return filepath.Join(dir, ".aileron", "session.log")
}

// openSessionLogger creates an slog.Logger that writes to
// .aileron/session.log at the given level. Returns the logger and a
// cleanup function to close the file. If the file cannot be created,
// returns a discard logger so callers never receive nil.
func openSessionLogger(dir string, level slog.Level) (*slog.Logger, func()) {
	path := SessionLogPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
		Level: level,
	}))
	return logger, func() { f.Close() }
}
