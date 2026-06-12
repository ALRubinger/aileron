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
	"runtime"
	"sort"
	"strings"
	"sync"
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
	// SandboxProxy controls whether the sandbox HTTPS proxy bootstrap
	// runs for this launch. Tri-state: "on" forces bootstrap (preflight
	// refuses launch if the image can't satisfy the BYO contract),
	// "off" disables it, and "auto" (or empty) defers to the default
	// for the resolved sandbox runtime (on for docker/podman).
	SandboxProxy string
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

// resolveSandboxMCPBinary locates an aileron-mcp binary suitable for
// bind-mounting into a Linux sandbox container.
//
// The host aileron-mcp produced by `task build:mcp` (or the official
// packages) is built for the host OS/arch. On Linux/<arch>, that binary
// runs unchanged inside a matching-arch container. On macOS or Windows
// hosts, mounting a Mach-O / PE binary into a Linux container produces
// `exec format error` at validate time (see runtime.go's "arch mismatch
// or corrupt mount" path), even when the container's arch matches the
// host's. To make sandbox launch work off Linux without per-user manual
// cross-compilation, `task build:mcp` also produces a Linux variant
// suffixed with the host GOARCH (e.g. `aileron-mcp-linux-arm64`); this
// resolver prefers that sibling when it exists and falls back to the
// host-arch binary otherwise.
//
// Container platform != host GOARCH (e.g. user passes
// `DOCKER_DEFAULT_PLATFORM=linux/amd64` on an arm64 host) remains a hard
// error at validate time; cross-architecture launch is out of scope for
// the local dev build path and is handled by the published per-agent
// images on the v4 roadmap.
func resolveSandboxMCPBinary(selfPath string) (string, error) {
	suffixed := "aileron-mcp-linux-" + runtime.GOARCH
	if bin, err := resolveSibling(selfPath, suffixed); err == nil {
		return bin, nil
	}
	return resolveMCPBinary(selfPath)
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
	mounts, cleanupMounts, err := sandboxRuntimeMounts(commandName, sandboxMCPBinName)
	if err != nil {
		return err
	}
	defer cleanupMounts()
	mounts = append(mounts, extraMounts...)
	// Resolve aileron-mcp and append its mount so the validate step
	// can smoke-check the binary inside the container (ADR-0024). Prefer
	// the cross-compiled Linux variant when present so this works off a
	// macOS/Windows host.
	selfPath, _ := os.Executable()
	mcpBinHost, err := resolveSandboxMCPBinary(selfPath)
	if err != nil {
		return err
	}
	mounts = append(mounts, sandboxcontainer.Volume{
		Source:   mcpBinHost,
		Target:   sandboxMCPBinPath,
		ReadOnly: true,
	})
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
		RequireMCPBinary:  true,
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
	proxyState := resolveSandboxProxyState(config.SandboxProxy, os.Getenv(sandboxProxyEnv), config.SandboxRuntime)
	if proxyState.Refuse {
		// User explicitly asked for proxy bootstrap against a sandbox
		// mode that cannot support it (e.g. --sandbox-proxy=on
		// --sandbox=off). Record the disabled event and fail before
		// touching the sandbox subsystem.
		reportSandboxProxyDisabled(ctx, client, "", proxyState.DisabledReason, config.SandboxRuntime, "")
		return LaunchResult{}, sandboxProxyRefuseError(proxyState.DisabledReason, config.SandboxRuntime)
	}
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
		if proxyState.Enabled {
			proxyBootstrap, err = prepareSandboxProxyBootstrap(stateDir, sessionID, agentEndpointURL, daemonToken)
			if err != nil {
				return LaunchResult{}, fmt.Errorf("prepare sandbox proxy bootstrap: %w", err)
			}
			applySandboxProxyBootstrapEnv(agentEnv, proxyBootstrap)
		}
	}
	if sandboxEnabled {
		if err := validateSandboxForLaunch(ctx, sandboxPlan, config, agentEnv, proxyBootstrap.Mounts...); err != nil {
			// Preflight failure when proxy bootstrap is active — refuse
			// to launch with an actionable error pointing at the BYO
			// image proxy contract and the opt-out flag, and record a
			// sandbox.proxy.disabled audit event with reason
			// preflight_failed.
			if proxyState.Enabled && isSandboxProxyContractFailure(err) {
				reportSandboxProxyDisabled(ctx, client, sessionID, sandboxProxyReasonPreflightFailed, config.SandboxRuntime, sandboxPlan.Image)
				return LaunchResult{}, sandboxProxyPreflightFailedError(sandboxPlan.Image, err)
			}
			return LaunchResult{}, err
		}
		if !proxyState.Enabled && proxyState.DisabledReason != "" {
			reportSandboxProxyDisabled(ctx, client, sessionID, proxyState.DisabledReason, config.SandboxRuntime, sandboxPlan.Image)
		}
	} else if proxyState.DisabledReason != "" {
		// Host launch path — record the unsupported-mode disabled
		// reason so audit covers every launch path.
		reportSandboxProxyDisabled(ctx, client, sessionID, proxyState.DisabledReason, config.SandboxRuntime, "")
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

	// Materialize the agent's AuthSpec into the container before
	// launchSandbox runs. EnvBindings merge into agentEnv (the in-
	// container agent inherits them); FileBindings + StaticFiles
	// produce writable bind-mounts that hold the rendered files.
	// Capture on clean exit snapshots in-container rotations back
	// to the vault. See ADR-0025.
	//
	// AuthSpec applies under sandbox launch only in v1 — host-launch
	// parity is a separate, smaller PR per the plan's scope boundary.
	var authPrep authSpecPrep
	if sandboxEnabled {
		// On Linux the host operator (UID typically 1000) owns the
		// transient auth dir, but the sandbox container runs as the
		// image's `agent` system user, so writable bind-mounted
		// credential files would be agent-unwritable. The hook chowns
		// the transient tree to the image's resolved agent UID; it is
		// nil on macOS/Windows where the Docker Desktop shim handles UID
		// translation. See ADR-0025 and issue #988.
		chownHook := newAgentDirChownHook(ctx, sandboxcontainer.DefaultRunner(),
			sandboxPlan.Runtime, sandboxPlan.Image)
		// The symmetric counterpart: after the container exits, chown the
		// transient tree back to the host operator so capture-on-exit can
		// read a credential the agent rotated as the agent UID. Without it,
		// rotations are silently dropped on rootful Docker Linux.
		reclaimHook := newTransientReclaimHook(ctx, sandboxcontainer.DefaultRunner(),
			sandboxPlan.Runtime, sandboxPlan.Image)
		prep, err := prepareAuthSpec(ctx, config.Agent.Name(), config.Agent.AuthSpec(),
			client, sessionLog, os.Stderr, chownHook, reclaimHook)
		if err != nil {
			return LaunchResult{}, fmt.Errorf("prepare agent auth spec: %w", err)
		}
		authPrep = prep
		defer authPrep.Cleanup()
		for k, v := range authPrep.EnvAdditions {
			agentEnv[k] = v
		}
		proxyBootstrap.Mounts = append(proxyBootstrap.Mounts, authPrep.Mounts...)
		if authPrep.HasBindings && !authPrep.RenderedAnyCredential {
			// Spec declared bindings but every one was a vault miss
			// (empty vault, no Required entries). Print the
			// bootstrap UX line per R30 so the user sees why the
			// agent is about to prompt for login. A static-file
			// mount (Claude's onboarding stub) does not count as a
			// rendered credential.
			fmt.Fprintf(os.Stderr, "[launcher] no credentials in vault for %s; agent will prompt for login\n",
				config.Agent.Name())
		}
	}

	// captureOnce wraps the AuthSpec Capture so the clean-exit gate
	// and the SIGINT/SIGTERM salvage path inside launchSandbox can
	// each request a Capture, but the in-container rotations are
	// written back to the vault exactly once even when a signal races
	// a clean exit (R15, ADR-0025). A nil CaptureFn (host launch, or
	// an agent with an empty AuthSpec) makes this a safe no-op.
	captureOnce := newSalvageCapture(authPrep.CaptureFn)

	// containerName makes the sandbox container addressable so the
	// salvage path can `<runtime> stop` it on a signal. Deterministic
	// per session so the run and the signal handler agree without a
	// shared variable.
	containerName := "aileron-sbx-" + sessionID

	var result LaunchResult
	var runErr error
	if sandboxEnabled {
		result, runErr = launchSandbox(ctx, sandboxPlan, config, agentEnv, containerName, captureOnce, sessionLog, proxyBootstrap.Mounts...)
	} else {
		result, runErr = launchHost(ctx, config, daemonURL, sessionID, agentEnv)
	}

	// Capture fires on clean container exit (R15) and on the
	// graceful-shutdown salvage path inside launchSandbox. Forcible
	// termination that does not route through the signal handler
	// (SIGKILL, runtime crash) skips Capture so the prior vault entry
	// is retained — the next launch self-heals via the agent's own
	// refresh or a manual `aileron vault put`. captureOnce dedupes a
	// signal-then-clean-exit race so the write lands exactly once.
	if sandboxEnabled && runErr == nil {
		captureOnce()
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
	extraArgs, mcpMounts, mcpErr := config.Agent.ConfigureMCP(mcpBin, mcpEnv, config.Dir, ModeHost)
	if mcpErr != nil {
		return LaunchResult{}, fmt.Errorf("configuring MCP for %s: %w", config.Agent.Name(), mcpErr)
	}
	if len(mcpMounts) > 0 {
		// Host launch has no container to mount into; an agent returning
		// mounts under ModeHost is a contract violation, not a runtime
		// fallback. Surface it loudly rather than silently dropping.
		return LaunchResult{}, fmt.Errorf("agent %s returned %d MCP mounts under host launch; mounts are sandbox-only", config.Agent.Name(), len(mcpMounts))
	}
	allArgs = append(allArgs, extraArgs...)

	cmd := exec.CommandContext(ctx, agentPath, allArgs...)
	cmd.Env = buildEnv(agentEnv)
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}
	return launchDirect(cmd, config)
}

func launchSandbox(ctx context.Context, plan SandboxLaunchPlan, config LaunchConfig, agentEnv map[string]string, containerName string, captureOnce func(), sessionLog *slog.Logger, extraMounts ...sandboxcontainer.Volume) (LaunchResult, error) {
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
	// Sandbox-flavored lookup picks the cross-compiled Linux variant
	// when present so this path works off macOS/Windows hosts without
	// per-user cross-compilation.
	selfPath, _ := os.Executable()
	mcpBinHost, err := resolveSandboxMCPBinary(selfPath)
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
	extraArgs, mcpMounts, mcpErr := config.Agent.ConfigureMCP(sandboxMCPBinPath, mcpEnv, config.Dir, ModeSandbox)
	if mcpErr != nil {
		return LaunchResult{}, fmt.Errorf("configuring MCP for %s: %w", config.Agent.Name(), mcpErr)
	}
	for _, m := range mcpMounts {
		mounts = append(mounts, sandboxcontainer.Volume{
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}

	command := append([]string{commandName}, config.Agent.Args()...)
	command = append(command, extraArgs...)
	command = append(command, config.Args...)
	user := ""
	if sandboxProxyBootstrapActive(agentEnv) {
		user = "root"
		command = append([]string{"aileron-run-with-proxy-ca"}, command...)
	}

	runOpts := sandboxcontainer.RunOptions{
		Runtime: plan.Runtime,
		Image:   plan.Image,
		WorkDir: config.Dir,
		Env:     agentEnv,
		Volumes: mounts,
		Command: command,
		User:    user,
		TTY:     term.IsTerminal(int(os.Stdin.Fd())),
		Name:    containerName,
	}

	// Graceful-shutdown salvage (ADR-0025, R13/R15): the one-shot
	// container runs in the foreground, so a Ctrl-C (SIGINT) or a
	// SIGTERM reaches the container process group and the runtime
	// force-kills it before the clean-exit Capture gate runs. Without
	// intervention any in-container OAuth rotation written to the
	// host-side bind-mount would never reach the vault. We install a
	// signal handler that, on the first SIGINT/SIGTERM, best-effort
	// stops the named container with a bounded grace window then runs
	// the same captureOnce the clean-exit gate uses. SIGKILL stays
	// uncatchable and intentionally bypasses this path.
	sigCh := make(chan os.Signal, 1)
	sandboxSignalNotify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	salvageDone := make(chan struct{})
	go func() {
		defer close(salvageDone)
		sig, ok := <-sigCh
		if !ok {
			// Channel closed by the clean-exit path below; no signal
			// arrived. The container exited on its own and the clean-
			// exit gate owns Capture, so there is nothing to salvage.
			return
		}
		// A SIGINT/SIGTERM is tearing the foreground container down
		// before the clean-exit Capture gate can run. Best-effort stop
		// the named container with a bounded grace window, then salvage
		// the credential files via the same once-guarded Capture. The
		// stop and Capture use Background-derived bounded contexts so a
		// cancelled parent ctx does not abort the salvage work itself.
		if sessionLog != nil {
			sessionLog.Info("sandbox graceful shutdown", "signal", sig.String(), "container", containerName)
		}
		stopCtx, cancelStop := context.WithTimeout(context.Background(), sandboxStopGrace+daemonHTTPTimeout)
		if err := sandboxStopContainer(stopCtx, plan.Runtime, containerName, sandboxStopGraceSeconds, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "[launcher] graceful stop of sandbox container %s failed: %v; salvaging credentials anyway\n", containerName, err)
			if sessionLog != nil {
				sessionLog.Warn("sandbox graceful stop failed", "container", containerName, "error", err)
			}
		}
		cancelStop()
		captureOnce()
	}()

	_, err = sandboxRunContainer(ctx, plan.Runtime, os.Stdout, os.Stderr, runOpts)

	// Stop signal delivery and drain the salvage goroutine before
	// returning so no post-return signal is caught and no goroutine
	// leaks (mirrors launchDirect's signal.Stop discipline). Closing
	// sigCh unblocks the goroutine on the clean-exit path; if a signal
	// already landed the goroutine is mid-salvage and salvageDone closes
	// once Capture completes.
	sandboxSignalStop(sigCh)
	close(sigCh)
	<-salvageDone

	if err != nil {
		return exitResult(err)
	}
	return LaunchResult{ExitCode: 0}, nil
}

// newSalvageCapture builds the once-guarded Capture wrapper shared by
// the clean-exit gate and the SIGINT/SIGTERM salvage path. A nil
// captureFn (host launch or an empty AuthSpec) yields a no-op; otherwise
// the first call runs captureFn with a fresh bounded context and any
// later call is dropped so a signal-then-clean-exit race writes the
// rotation back to the vault exactly once.
func newSalvageCapture(captureFn func(context.Context)) func() {
	if captureFn == nil {
		return func() {}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), daemonHTTPTimeout)
			captureFn(ctx)
			cancel()
		})
	}
}

// sandboxStopGraceSeconds is the bounded SIGTERM-to-SIGKILL grace window
// passed to `<runtime> stop --time`. Fixed at 10s per ADR-0025.
const sandboxStopGraceSeconds = 10

// sandboxStopGrace mirrors sandboxStopGraceSeconds as a Duration for the
// host-side context budget around the stop call.
const sandboxStopGrace = sandboxStopGraceSeconds * time.Second

// sandboxRunContainer runs the sandbox container. It is a package
// variable so tests inject a fake that blocks until a simulated signal
// fires, exercising the salvage path without an OS signal or a real
// container runtime.
var sandboxRunContainer = func(ctx context.Context, runtimeName string, stdout, stderr io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Stdout:  stdout,
		Stderr:  stderr,
	}.Run(ctx, opts)
}

// sandboxStopContainer issues the graceful stop. A package variable so
// tests observe the stop call (and inject failures) deterministically.
var sandboxStopContainer = func(ctx context.Context, runtimeName, name string, graceSeconds int, stdout, stderr io.Writer) error {
	return sandboxcontainer.StopContainer(ctx, nil, runtimeName, name, graceSeconds, stdout, stderr)
}

// sandboxSignalNotify and sandboxSignalStop wrap signal.Notify/Stop so
// tests drive the salvage path by sending on the injected channel
// instead of raising a real OS signal at the test runner. Defaults
// forward to the os/signal package unchanged.
var sandboxSignalNotify = func(ch chan<- os.Signal, sigs ...os.Signal) {
	signal.Notify(ch, sigs...)
}

var sandboxSignalStop = func(ch chan<- os.Signal) {
	signal.Stop(ch)
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
