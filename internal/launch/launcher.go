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
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
	skillstore "github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/proxybinding"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	sandboxtoolchain "github.com/ALRubinger/aileron/internal/sandbox/toolchain"
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
	sandboxProxyCAPath = "/etc/aileron/proxy/ca.pem"
	// sandboxMCPBinPath is where the launcher bind-mounts the host-built
	// aileron-mcp binary inside the container. The agent's MCP client
	// execs this path as its stdio child. See ADR-0024.
	sandboxMCPBinPath = "/usr/local/bin/aileron-mcp"
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
	// today's direct host launch path. "auto" and "docker" prepare the
	// composition-selected image and execute the agent in a one-shot
	// container.
	SandboxRuntime string
	// SandboxBuildPolicy controls launch-time builds for buildable
	// sandbox tiers. Empty defaults to auto for launch.
	SandboxBuildPolicy string
	// SandboxProxy controls whether the sandbox HTTPS proxy bootstrap
	// runs for this launch. Tri-state: "on" forces bootstrap (preflight
	// refuses launch if the image can't satisfy the BYO contract),
	// "off" disables it, and "auto" (or empty) defers to the default
	// for the resolved sandbox runtime (on for docker).
	SandboxProxy string
	// HostLogin controls whether the launcher runs a binding's host-side
	// credential acquirer (FileBinding.HostAcquire) on a vault miss.
	// Tri-state: "on" or "auto" (or empty) enable host acquisition when
	// a binding declares one; "off" forces the legacy in-container login
	// path even for bindings that declare an acquirer. Overridable via
	// the AILERON_LAUNCH_HOST_LOGIN env var.
	HostLogin string
}

// hostLoginEnv is the environment variable that overrides the
// --host-login flag's default. Values follow the same on|off|auto
// tri-state as the flag.
const hostLoginEnv = "AILERON_LAUNCH_HOST_LOGIN"

// NoVaultCredentialMsgFormat is the R30 bootstrap line the launcher
// prints when an AuthSpec declares file bindings but every one was a
// vault miss, so the agent will run its in-container login on first
// launch. It carries a single %s for the agent name. A successful
// host-side acquire renders the credential and suppresses this line,
// so the message is the observable signal that the silent host-seed
// path did NOT run. Tests assert against this constant rather than a
// re-typed literal so the launcher and its coverage cannot drift, and
// ADR-0025 quotes this exact text.
const NoVaultCredentialMsgFormat = "[launcher] no credentials in vault for %s; agent will prompt for login\n"

// resolveHostLogin computes whether host-side credential acquisition is
// enabled for a launch, given the user's flag and env-var inputs. The
// precedence is flag > env > default, with "auto" (or empty, or an
// unrecognized value) deferring to the next source. The default is
// enabled: host acquisition only activates when a binding actually
// declares a HostAcquire hook, so the safe default is to honor it.
// Only an explicit "off" (on the flag, or via the env when the flag is
// auto) forces the legacy in-container path.
func resolveHostLogin(flagValue, envValue string) bool {
	mode := normalizeSandboxProxyTriState(flagValue)
	if mode == "" || mode == "auto" {
		envMode := normalizeSandboxProxyTriState(envValue)
		if envMode != "" {
			mode = envMode
		}
	}
	return mode != "off"
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
	// CachePaths carries the Aileron-managed cache-volume declarations from the
	// project's customizations.aileron block. launchSandbox turns each entry
	// into a deterministically named Docker volume mounted at ContainerPath.
	CachePaths []sandboxcomposition.CachePath
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

// goosExeSuffix is the executable file extension for the given target GOOS.
// On Windows the aileron and aileron-mcp binaries are written as
// `aileron.exe` / `aileron-mcp.exe`, so a bare sibling name like
// "aileron-mcp" never resolves on disk; everywhere else it is empty.
//
// The GOOS is a parameter (not read from runtime.GOOS) so the Windows
// extension can be applied to a cross-target candidate — e.g. a
// `windows/amd64` managed-binary name on a macOS build host — and so the
// rule is unit-testable for every target from any host.
func goosExeSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// exeSuffix is the host platform's executable file extension. It is the
// host-bound convenience wrapper over goosExeSuffix; callers that probe
// for a sibling of the running binary (same OS as the host) use it.
func exeSuffix() string {
	return goosExeSuffix(runtime.GOOS)
}

// archSuffixedCandidates returns the on-disk filenames to probe for a
// managed binary that is published per platform as `<name>-<goos>-<goarch>`
// (with the target-GOOS executable suffix), in priority order.
//
// The published, platform-suffixed filename is tried first because that is
// the real name a cross-platform build produces (e.g.
// `aileron-mcp-linux-arm64`, `aileron-mcp-windows-amd64.exe`). The bare
// `name` is still probed last so a manually placed, extension-less binary
// resolves too — the same fallback contract siblingCandidates provides.
//
// goos and goarch are parameters (not read from runtime.GOOS/GOARCH) so
// the candidate names for all M target platforms are computed — and
// unit-tested — from a single host: CI runs on one OS, but the windows
// `.exe` suffix and the darwin/linux ordering must all be verifiable. This
// mirrors siblingCandidates' design exactly, where the platform axis is
// injected rather than read from the runtime.
//
// If name already carries the target suffix it is not double-suffixed
// (mirrors siblingCandidates' already-suffixed guard).
func archSuffixedCandidates(name, goos, goarch string) []string {
	suffixed := name + "-" + goos + "-" + goarch
	exe := goosExeSuffix(goos)
	if exe != "" && !strings.HasSuffix(suffixed, exe) {
		suffixed += exe
	}
	return []string{suffixed, name}
}

// siblingCandidates returns the on-disk filenames to probe for a sibling
// binary, in priority order. With a non-empty platform suffix (".exe" on
// Windows) the suffixed filename is tried first because that is the real
// name the Windows build produces; the bare name is still probed so a
// manually placed extension-less binary resolves too. Off Windows the
// suffix is empty and the single bare name is returned. The suffix is
// passed in (rather than read from runtime.GOOS) so the Windows ordering
// is unit-testable from any host.
func siblingCandidates(name, suffix string) []string {
	if suffix != "" && !strings.HasSuffix(name, suffix) {
		return []string{name + suffix, name}
	}
	return []string{name}
}

// resolveSibling looks for a binary next to the given executable path,
// then falls back to searching PATH.
//
// On Windows the on-disk sibling carries an `.exe` extension while the
// caller passes the bare name (e.g. "aileron-mcp"), so the sibling
// candidate is probed both with and without the platform exeSuffix; a
// bare-name os.Stat would otherwise miss `aileron-mcp.exe`, fall
// through, and let the launcher write an unresolvable
// `command = "aileron-mcp"` into Codex's config.toml — which fails at
// MCP startup with OS error 2 (issue #1387). exec.LookPath is the final
// fallback and already applies %PATHEXT% on Windows, so it needs no
// extra handling.
func resolveSibling(selfPath, name string) (string, error) {
	// Absolutize once so the sibling candidate (and any shim target
	// resolved relative to it) is absolute without a per-candidate
	// filepath.Abs guard. A relative selfPath (e.g. os.Args[0]) is
	// resolved against the cwd here.
	if abs, err := filepath.Abs(selfPath); err == nil {
		selfPath = abs
	}
	dir := filepath.Dir(selfPath)
	for _, c := range siblingCandidates(name, exeSuffix()) {
		candidate := filepath.Join(dir, c)
		if _, err := os.Stat(candidate); err == nil {
			return resolveReparsePoints(resolveShimTarget(candidate)), nil
		}
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return resolveReparsePoints(resolveShimTarget(found)), nil
}

// resolveReparsePoints collapses symlinks and Windows directory junctions
// in path to a concrete on-disk path, best-effort.
//
// Scoop exposes an installed binary through an
// `...\apps\<app>\current\...` directory junction (a reparse point) that
// points at the versioned install dir. resolveShimTarget unwraps the
// Scoop stub to that junction path; a normal shell launch traverses it
// fine, but Codex's elevated Windows sandbox cannot spawn a command whose
// path crosses the junction and still fails with `OS error 2` even after
// the stub is unwrapped — the successor symptom to #1405, where writing
// the real (but junction-rooted) target was necessary yet not sufficient.
// The command written into the agent's MCP config must therefore be the
// resolved versioned path (`...\apps\<app>\0.0.9\...`). EvalSymlinks
// resolves junctions as well as symlinks on Windows.
//
// On failure (the path does not exist, or the platform cannot evaluate
// it) the input is returned unchanged so a working path is never made
// worse. The behavior is keyed off on-disk reparse data, not GOOS, so a
// symlink planted in a test exercises it on any host.
func resolveReparsePoints(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// resolveShimTarget unwraps a Scoop shim to the real binary it launches.
//
// On a Scoop install (Windows), `aileron-mcp.exe` on PATH / next to
// `aileron.exe` is not the real binary but a ~133 KB kiennq launcher
// stub. Scoop records the real target in a co-located `<name>.shim`
// sidecar as `path = "...\apps\aileron\current\aileron-mcp.exe"`. The
// stub re-execs that target and works fine standalone, but an agent that
// spawns the command under a Windows sandbox (e.g. Codex with
// `[windows] sandbox = "elevated"`) fails to launch the stub with
// `OS error 2` (issue #1405). Writing the real target into the agent's
// MCP config instead of the shim path avoids the indirection so the MCP
// server starts on first launch.
//
// When no sidecar is present, or it cannot be read/parsed, or the target
// it names does not exist on disk, the original path is returned
// unchanged — this is a best-effort unwrap that never makes a working
// path worse. The lookup is keyed off the sidecar (not GOOS) so a shim
// layout planted in a test resolves on any host.
//
// binPath is expected to be absolute (resolveSibling absolutizes before
// calling). A relative `path` value in the sidecar is resolved against
// binPath's directory, so the returned target is absolute too.
func resolveShimTarget(binPath string) string {
	shimPath := strings.TrimSuffix(binPath, filepath.Ext(binPath)) + ".shim"
	data, err := os.ReadFile(shimPath)
	if err != nil {
		return binPath
	}
	target := parseShimPath(string(data))
	if target == "" {
		return binPath
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(binPath), target)
	}
	if _, err := os.Stat(target); err != nil {
		return binPath
	}
	return target
}

// parseShimPath extracts the real binary path from the contents of a
// Scoop `.shim` sidecar, whose body is an INI-like list of `key = value`
// lines. It returns the value of the first `path` key (surrounding
// double quotes stripped), or "" when no `path` key is present.
func parseShimPath(contents string) string {
	for _, line := range strings.Split(contents, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) != "path" {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		return value
	}
	return ""
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

// resolveManagedBinary locates a managed binary published per platform as
// `<name>-<goos>-<goarch>` next to the running aileron binary, for the
// given target platform.
//
// It asks archSuffixedCandidates for the ordered on-disk filenames to
// probe (the `<name>-<goos>-<goarch>` form first, then the bare name) and
// hands each to resolveSibling, which does the actual stat / PATH search
// and unwraps Scoop `.shim` sidecars and `current` junctions. Because the
// probe flows through resolveSibling unchanged, the Windows `.exe`,
// reparse-point, and Scoop-shim handling is preserved for free; this
// helper only decides *which* candidate names to look for, in what order.
//
// goos/goarch are the *target* platform, passed in so a managed binary for
// any of the M supported platforms can be located from any host (the
// platform axis is a parameter, mirroring archSuffixedCandidates and
// siblingCandidates — independent testability from a single CI host is the
// reason). The first candidate that resolves wins; the last error is
// returned when none do.
//
// This is the shared selection seam reused to locate the managed Node
// binary (sub-issues 1/4) per platform; no binary-specific logic lives
// here.
func resolveManagedBinary(selfPath, name, goos, goarch string) (string, error) {
	var lastErr error
	for _, candidate := range archSuffixedCandidates(name, goos, goarch) {
		bin, err := resolveSibling(selfPath, candidate)
		if err == nil {
			return bin, nil
		}
		lastErr = err
	}
	return "", lastErr
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
// The platform-suffixed candidate construction now flows through the
// shared, M-way archSuffixedCandidates / resolveManagedBinary unit (the
// target is always linux/<host-goarch> for the sandbox case), rather than
// a hardcoded `aileron-mcp-linux-<arch>` string; the aileron-mcp contract
// is unchanged. When neither the Linux sibling nor a bare-name candidate
// resolves through resolveManagedBinary, the lookup falls back to
// resolveMCPBinary so the plain host binary (the Linux-host case) still
// resolves and the "not found" remediation hint is preserved.
//
// Container platform != host GOARCH (e.g. user passes
// `DOCKER_DEFAULT_PLATFORM=linux/amd64` on an arm64 host) remains a hard
// error at validate time; cross-architecture launch is out of scope for
// the local dev build path and is handled by the published per-agent
// images on the v4 roadmap.
func resolveSandboxMCPBinary(selfPath string) (string, error) {
	if bin, err := resolveManagedBinary(selfPath, "aileron-mcp", "linux", runtime.GOARCH); err == nil {
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
	case "", "off", sandboxcontainer.DefaultRuntime, "docker":
		return nil
	default:
		return fmt.Errorf("unsupported sandbox runtime %q (want off, auto, or docker)", runtimeName)
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

func prepareSandbox(ctx context.Context, workDir, agent, runtimeName, buildPolicy string, stdout, stderr io.Writer) (SandboxLaunchPlan, error) {
	if err := validateSandboxRuntime(runtimeName); err != nil {
		return SandboxLaunchPlan{}, err
	}
	policy, err := normalizeLaunchBuildPolicy(buildPolicy)
	if err != nil {
		return SandboxLaunchPlan{}, err
	}
	plan, err := sandboxcomposition.Discover(workDir, version.Version, agent)
	if err != nil {
		return SandboxLaunchPlan{}, err
	}
	// Launch resolves the toolchain via env only (no flag); the default is the
	// managed toolchain (#1530), with AILERON_SANDBOX_TOOLCHAIN=host-npx as the
	// opt-out. The flag value is always empty here, so resolution is env → default,
	// which wires the managed provisioner by default below.
	toolchainMode, nodeBinary, cliEntrypoint := sandboxcontainer.ResolveToolchainSelection("", "", "", os.Getenv)
	builder := sandboxcontainer.Builder{
		Runtime: runtimeName,
		Stdout:  stdout,
		Stderr:  stderr,
	}
	if sandboxcontainer.IsManagedToolchain(toolchainMode) {
		builder.Provisioner = sandboxtoolchain.Provisioner{}
	}
	result, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		WorkDir:                   workDir,
		Plan:                      plan,
		Policy:                    policy,
		ToolchainMode:             toolchainMode,
		NodeBinary:                nodeBinary,
		DevcontainerCLIEntrypoint: cliEntrypoint,
	})
	if errors.Is(err, sandboxcontainer.ErrNoBuildRequired) {
		runtime, runtimeErr := sandboxcontainer.ResolveRuntime(runtimeName)
		if runtimeErr != nil {
			return SandboxLaunchPlan{}, runtimeErr
		}
		return SandboxLaunchPlan{
			Runtime:    runtime,
			Image:      result.Image,
			Tier:       result.Tier,
			CachePaths: plan.Aileron.CachePaths,
		}, nil
	}
	if err != nil {
		return SandboxLaunchPlan{}, err
	}
	return SandboxLaunchPlan{
		Runtime:    result.Runtime,
		Image:      result.Image,
		Tier:       result.Tier,
		Built:      result.Built,
		CachePaths: plan.Aileron.CachePaths,
	}, nil
}

var prepareSandboxForLaunch = prepareSandbox

func validateSandbox(ctx context.Context, plan SandboxLaunchPlan, config LaunchConfig, agentEnv map[string]string, extraMounts ...sandboxcontainer.Volume) error {
	commandName := firstAgentBinary(config.Agent)
	if commandName == "" {
		return fmt.Errorf("agent %q has no container command", config.Agent.Name())
	}
	mounts, err := sandboxRuntimeMounts()
	if err != nil {
		return err
	}
	mounts = append(mounts, extraMounts...)
	// Unbaked image: resolve aileron-mcp on the host and append its mount so
	// the validate step can smoke-check the binary inside the container
	// (ADR-0024). Prefer the cross-compiled Linux variant when present so
	// this works off a macOS/Windows host.
	//
	// Baked image (issue #957): aileron-mcp already lives at
	// sandboxMCPBinPath, so skip host resolution (a missing host binary is
	// not fatal on a sealed runtime) and skip the host-mount. RequireMCPBinary
	// still smoke-checks the in-image binary below.
	if sandboxBakedMCPVersion(ctx, plan.Runtime, plan.Image) == "" {
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
	}
	if err := validateSandboxImageForLaunch(ctx, plan, config, agentEnv, mounts, commandName); err != nil {
		err = sandboxcomposition.EnrichValidateError(err, plan.Tier, config.Agent.Name(), version.Version, config.Dir, runtime.GOOS, sandboxcontainer.WorkspaceRelabelActive(plan.Runtime))
		return fmt.Errorf("sandbox image %s is not launchable for %s: %w", plan.Image, config.Agent.Name(), err)
	}
	return nil
}

func validateSandboxImage(ctx context.Context, plan SandboxLaunchPlan, config LaunchConfig, agentEnv map[string]string, mounts []sandboxcontainer.Volume, commandName string) error {
	if err := (sandboxcontainer.Builder{
		Runtime: plan.Runtime,
		Stdout:  io.Discard,
	}).Validate(ctx, sandboxcontainer.ValidateOptions{
		Runtime:           plan.Runtime,
		Image:             plan.Image,
		WorkDir:           config.Dir,
		Env:               agentEnv,
		Volumes:           mounts,
		Command:           []string{commandName},
		RequireProxyTrust: agentEnv["AILERON_SANDBOX_PROXY_MODE"] != "",
		RequireMCPBinary:  true,
		RemapWorkspaceUID: workspaceUIDRemapActive(plan.Runtime),
	}); err != nil {
		return err
	}
	return nil
}

var validateSandboxForLaunch = validateSandbox
var validateSandboxImageForLaunch = validateSandboxImage

// sandboxBakedMCPVersion reports the aileron-mcp version baked into the
// resolved sandbox image, or "" when the image is unbaked (no
// ai.aileron.mcp.version label) or cannot be inspected. A non-empty result
// means the image already provides aileron-mcp at sandboxMCPBinPath, so the
// launcher skips host-binary resolution and the host bind-mount (issue #957),
// letting sealed customer-operated runtimes launch without a host aileron-mcp.
// Swappable in tests. validateSandbox and launchSandbox each call it once per
// launch; an `image inspect` is a cheap local metadata read, so two calls per
// launch is accepted rather than threading one result through the call chain.
var sandboxBakedMCPVersion = func(ctx context.Context, runtimeName, image string) string {
	return sandboxcontainer.BakedMCPVersion(ctx, sandboxcontainer.DefaultRunner(), runtimeName, image)
}

// preflightHostBindings validates the merged host-binding descriptor table
// (built-in defaults overlaid with the user layer) before the agent
// environment boots. It reuses the loader's existing validation output (no
// new validation): a `<...>` placeholder in any descriptor field returns the
// loader's file/entry/field-named error, which fails the launch; non-fatal
// suspect-shape warnings (e.g. a sigv4-resign access_key_id that does not
// match the AWS key shape) are written to w and never block the launch.
//
// Without this preflight a placeholder only fails at daemon boot or
// incidentally via the gh-gated sentinel-swap path, so a bad athena/sigv4
// descriptor surfaces minutes later as an opaque upstream auth failure
// instead of at launch time.
func preflightHostBindings(w io.Writer) error {
	_, warnings, err := proxybinding.LoadHostBindingsWithWarnings(proxybinding.DefaultLoadOptions())
	if err != nil {
		return fmt.Errorf("host-binding descriptor preflight: %w", err)
	}
	for _, warning := range warnings {
		fmt.Fprintf(w, "aileron: host-binding warning: %s\n", warning)
	}
	return nil
}

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
	// Preflight the host-binding descriptors before the environment boots.
	// A copy-paste placeholder (e.g. a sigv4-resign access_key_id left as
	// "<AccessKeyId>") hard-fails the launch here with the loader's
	// file/entry/field-named error, instead of surfacing minutes later as
	// an opaque upstream auth failure (an AWS UnrecognizedClientException).
	// Non-fatal suspect-shape warnings are printed to stderr.
	if err := preflightHostBindings(os.Stderr); err != nil {
		return LaunchResult{}, err
	}
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
	resolveCtx, cancelResolve := context.WithTimeout(ctx, spawn.ResolveContextBudget)
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
		plan, err := prepareSandboxForLaunch(ctx, config.Dir, config.Agent.Name(), config.SandboxRuntime, config.SandboxBuildPolicy, os.Stdout, os.Stderr)
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
	reg, err := client.RegisterSession(regCtx, config.Agent.Name(), config.Dir)
	cancelReg()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("register session: %w", err)
	}
	sessionID := reg.ID
	// Session-scoped caveat token (ADR-0024, #958): the credential the
	// in-container aileron-mcp carries. It grants only the minimal /v1/*
	// surface (list/run actions, poll approvals, this session's comms),
	// so a leaked container token can never exercise full daemon
	// authority. The master daemonToken stays host-side. On an
	// unprotected daemon the caveat is empty and we fall back to the
	// (also empty) master token — no bearer is required there.
	containerMCPToken := reg.CaveatToken
	if containerMCPToken == "" {
		containerMCPToken = daemonToken
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
		agentEndpointURL = ContainerURLForRuntime(daemonURL, sandboxPlan.Runtime)
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
		// Disable Claude Code's in-container auto-updater. The sandbox
		// image installs the agent into a root-owned global prefix
		// (`npm install -g`), so the non-root agent user's update write
		// fails with "no write permission to npm prefix". Self-updating
		// inside an ephemeral container is also pointless — the image is
		// the unit of versioning, so a successful update would be lost on
		// the next launch anyway. The var is Claude-specific; other
		// agents ignore it. Host launch is unaffected (this block is
		// sandbox-only), so a user's own writable install still updates.
		agentEnv["DISABLE_AUTOUPDATER"] = "1"
		// AILERON_TOKEN in the container is the session-scoped caveat
		// token, not the master daemon token. aileron-mcp reads it and
		// authenticates to the daemon's /v1/* surface with only the
		// capabilities the agent uses, bound to this session.
		if containerMCPToken != "" {
			agentEnv["AILERON_TOKEN"] = containerMCPToken
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
		hostLoginEnabled := resolveHostLogin(config.HostLogin, os.Getenv(hostLoginEnv))
		prep, err := prepareAuthSpec(ctx, config.Agent.Name(), config.Agent.AuthSpec(),
			client, sessionLog, os.Stderr, chownHook, reclaimHook, hostLoginEnabled)
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
			fmt.Fprintf(os.Stderr, NoVaultCredentialMsgFormat, config.Agent.Name())
		}

		// Claude active-mode banner (#1340). Gated on the sandbox path
		// because the auth mode is inert on host launch (AuthSpec is only
		// materialized under sandbox), so emitting it there would be a false
		// signal (P0). Non-claude agents are a no-op.
		printClaudeAuthBanner(os.Stderr, config.Agent, sandboxEnabled, authPrep.RenderedAnyCredential)

		// User-level GitHub injection runs on every sandbox launch,
		// independent of the per-agent AuthSpec: it probes the
		// agent-independent `user/github` token in the vault and, when
		// present, exports a NON-SECRET sentinel GH_TOKEN and mounts a
		// secret-free git credential-helper gitconfig at
		// /home/agent/.gitconfig. The real token never enters the
		// container: `gh` issues its request because the sentinel passes
		// its local validation, and the daemon swaps the sentinel for the
		// real credential at the TLS boundary (sentinel-swap);
		// git-over-HTTPS emits an unauthenticated request the daemon seals
		// the same way (inject). A missing entry or locked vault
		// is a clean, non-fatal skip — the launch proceeds with GitHub
		// operations unauthenticated. The chown hook mirrors the AuthSpec
		// one: on rootful Docker Linux the host operator owns the
		// transient dir, so the tree is chowned to the image's agent UID
		// before mounting. See #1149.
		ghChownHook := newAgentDirChownHook(ctx, sandboxcontainer.DefaultRunner(),
			sandboxPlan.Runtime, sandboxPlan.Image)
		// gh's sealing bindings ship in its devcontainer Feature CLI unit
		// (#1323), not a central default. resolveGitHubSentinelSwapBindings reads
		// the image-derived unit layer the same way the daemon does
		// (assembleHostBindings) and returns the sentinel-swap subset, so the
		// launch-side plant and the proxy-side swap read one source of truth
		// across the process boundary.
		sentinelSwapBindings, err := resolveGitHubSentinelSwapBindings(ctx, sandboxPlan.Runtime, sandboxPlan.Image)
		if err != nil {
			return LaunchResult{}, err
		}
		ghPrep, err := prepareGitHubInject(ctx, client, sentinelSwapBindings, sessionLog, os.Stderr, ghChownHook)
		if err != nil {
			return LaunchResult{}, fmt.Errorf("prepare github inject: %w", err)
		}
		defer ghPrep.Cleanup()
		for k, v := range ghPrep.EnvAdditions {
			agentEnv[k] = v
		}
		proxyBootstrap.Mounts = append(proxyBootstrap.Mounts, ghPrep.Mounts...)
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
		result, runErr = launchSandbox(ctx, sandboxPlan, config, agentEnv, containerName, captureOnce, authPrep.ChownFn, sessionLog, proxyBootstrap.Mounts...)
	} else {
		result, runErr = launchHost(ctx, config, daemonURL, sessionID, daemonToken, agentEnv)
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

// ContainerURLForRuntime rewrites a daemon URL whose host is a loopback
// address (localhost / 127.0.0.1 / ::1) to host.docker.internal so a process
// inside a container can reach the host-bound daemon. Non-loopback and
// unparseable URLs are returned unchanged. runtimeName is retained as the
// runtime seam; v4 is Docker-only, so the loopback host always rewrites to
// host.docker.internal. It is exported so the tool-container proxy
// bootstrap can compute the container-facing proxy URL without duplicating the
// loopback-rewrite rule.
func ContainerURLForRuntime(rawURL, runtimeName string) string {
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
	// runtimeName is retained as the runtime seam; v4 is Docker-only, so
	// the loopback host always rewrites to host.docker.internal.
	host = "host.docker.internal"
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

func launchHost(ctx context.Context, config LaunchConfig, daemonURL, sessionID, daemonToken string, agentEnv map[string]string) (LaunchResult, error) {
	agentPath, err := ResolveBinary(config.Agent.BinaryNames())
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent %q: %w", config.Agent.Name(), err)
	}

	// Resolve aileron-mcp up front for the host launch path. The sandbox
	// path resolves its own (cross-compiled) aileron-mcp in launchSandbox.
	selfPath, _ := os.Executable()
	mcpBin, err := resolveMCPBinary(selfPath)
	if err != nil {
		return LaunchResult{}, err
	}

	allArgs := append(config.Agent.Args(ModeHost), config.Args...)
	mcpEnv := hostMCPEnv(daemonURL, sessionID, daemonToken)
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

// collapseNestedMCPMounts merges an agent's ConfigureMCP mounts into the
// existing volume list, relocating any MCP *file* mount whose target sits
// strictly inside an already-mounted directory rather than adding it as a
// separate nested bind mount.
//
// Nested file-inside-directory bind mounts are the failure mode behind
// issue #1143: Codex's AuthSpec emits a writable directory mount at
// /home/agent/.codex/ (so the in-container login can write auth.json),
// while ConfigureMCP emits a separate single-file mount of config.toml at
// /home/agent/.codex/config.toml. On macOS Docker Desktop (virtiofs), runc
// rejects creating the inner mountpoint with "mountpoint ... is outside of
// rootfs" and container init dies. Collapsing the file into the parent
// directory's host-side source means a single directory mount carries both
// files and no nested bind exists.
//
// When an MCP mount's target is not nested under any existing directory
// mount, it is appended unchanged — preserving the prior behavior for every
// agent that does not collide with an AuthSpec directory mount.
func collapseNestedMCPMounts(existing []sandboxcontainer.Volume, mcpMounts []MCPMount) ([]sandboxcontainer.Volume, error) {
	out := existing
	for _, m := range mcpMounts {
		parent := enclosingDirMount(out, m.Target)
		if parent == nil {
			out = append(out, sandboxcontainer.Volume{
				Source:   m.Source,
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
			})
			continue
		}
		// Relocate the file into the parent directory mount's host-side
		// source so the single directory mount exposes it at m.Target.
		// path.Base mirrors the in-container layout the agent reads.
		rel := strings.TrimPrefix(m.Target, parent.Target)
		rel = strings.TrimPrefix(rel, "/")
		dst := filepath.Join(parent.Source, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, fmt.Errorf("create parent dir for %s: %w", dst, err)
		}
		data, err := os.ReadFile(m.Source)
		if err != nil {
			return nil, fmt.Errorf("read MCP config %s: %w", m.Source, err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, fmt.Errorf("write MCP config into %s: %w", dst, err)
		}
	}
	return out, nil
}

// enclosingDirMount returns the volume in mounts whose target is a
// directory strictly containing target, or nil when target is not nested
// inside any of them. A directory mount is identified by target shape: an
// MCP single-file mount and a directory mount are distinguished only by
// whether target is a strict path prefix of another mount's target.
func enclosingDirMount(mounts []sandboxcontainer.Volume, target string) *sandboxcontainer.Volume {
	clean := path.Clean(target)
	for i := range mounts {
		dir := path.Clean(mounts[i].Target)
		if dir == clean {
			continue
		}
		if strings.HasPrefix(clean, dir+"/") {
			return &mounts[i]
		}
	}
	return nil
}

// skillStoreDir is a package-level seam so tests can point the skills
// projection at a temp store instead of the user's ~/.aileron/skills.
var skillStoreDir = skillstore.DefaultDir

// skillsStoreNonEmpty reports whether the canonical skill store at dir
// holds at least one installed skill. An absent or empty store means
// nothing to project.
func skillsStoreNonEmpty(dir string) bool {
	names, err := skillstore.New(dir).List()
	return err == nil && len(names) > 0
}

// appendSkillsMount projects the canonical skill store read-only at the
// agent's skills path. It is a no-op when the agent declares no skills path
// (SkillsPath() == "") or the store is empty.
//
// When the agent's skills target is NOT nested inside an existing mount,
// the store is added as a single read-only directory bind mount. When the
// target nests inside an existing (AuthSpec) directory mount, the store
// content is copied into that mount's host-side source so no nested bind
// mount is created — the same hazard avoidance collapseNestedMCPMounts
// applies for MCP config files (issue #1143: nested binds are rejected by
// runc on macOS Docker Desktop virtiofs). The copy lands under the relative
// subpath the target sits at within the parent mount.
func appendSkillsMount(existing []sandboxcontainer.Volume, agent Agent, storeDir string) ([]sandboxcontainer.Volume, error) {
	target := agent.SkillsPath()
	if target == "" {
		return existing, nil
	}
	if !skillsStoreNonEmpty(storeDir) {
		return existing, nil
	}

	parent := enclosingDirMount(existing, target)
	if parent == nil {
		// No enclosing mount: a plain read-only directory bind is safe.
		return append(existing, sandboxcontainer.Volume{
			Source:   storeDir,
			Target:   target,
			ReadOnly: true,
		}), nil
	}

	// The target nests inside an existing directory mount. Copy the store
	// content into that mount's host-side source under the relative subpath
	// rather than creating a nested bind. The agent reads the same files at
	// the same in-container path; nesting is avoided.
	//
	// Read-only semantics in the nested case: the enclosing AuthSpec mount
	// may be writable (Claude's /home/agent/.claude/ is, for credential
	// rotation), so the copied skills are not RO at the mount level. The
	// canonical store is still protected because this is a per-launch COPY,
	// not a bind back to ~/.aileron/skills: any in-container edit dies with
	// the ephemeral container and never reaches the canonical store.
	rel := strings.TrimPrefix(target, parent.Target)
	rel = strings.TrimPrefix(rel, "/")
	dst := filepath.Join(parent.Source, rel)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, fmt.Errorf("create skills dir %s: %w", dst, err)
	}
	if err := copySkillTree(storeDir, dst); err != nil {
		return nil, err
	}
	return existing, nil
}

// copySkillTree recursively copies the regular files and subdirectories
// under src into dst. Symlinks and other non-regular files are skipped.
func copySkillTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		default:
			return nil
		}
	})
}

// chownAuthTree, when non-nil, recursively chowns the AuthSpec transient
// tree to the resolved in-container agent UID. The launcher invokes it
// AFTER collapseNestedMCPMounts has placed the agent's MCP config into
// that tree, so the single delegated chown re-owns config.toml alongside
// auth.json. Running the chown before the placement re-owned the tree to
// the foreign agent UID with mode 0700 first, which made the unprivileged
// host-side MkdirAll/WriteFile in the collapse fail with EPERM on rootful
// Docker Linux (issue #1488). A nil chownAuthTree is a safe no-op (host
// launch, non-Linux host, or an agent with no AuthSpec).
func launchSandbox(ctx context.Context, plan SandboxLaunchPlan, config LaunchConfig, agentEnv map[string]string, containerName string, captureOnce func(), chownAuthTree func() error, sessionLog *slog.Logger, extraMounts ...sandboxcontainer.Volume) (LaunchResult, error) {
	commandName := firstAgentBinary(config.Agent)
	if commandName == "" {
		return LaunchResult{}, fmt.Errorf("agent %q has no container command", config.Agent.Name())
	}
	mounts, err := sandboxRuntimeMounts()
	if err != nil {
		return LaunchResult{}, err
	}
	mounts = append(mounts, extraMounts...)

	// Aileron-managed cache volumes: each declared (CLI, identity) cache becomes
	// a deterministically named Docker volume mounted read-write at its
	// container path. Docker auto-creates the volume on first mount and persists
	// it across launches; re-auth to a different identity keys a fresh volume.
	cacheMounts, err := sandboxCacheVolumes(plan.CachePaths)
	if err != nil {
		return LaunchResult{}, err
	}
	mounts = append(mounts, cacheMounts...)

	// Unbaked image: resolve aileron-mcp on the host and bind-mount it
	// read-only at the container's well-known MCP binary path. Matches the
	// resolution shape host launch uses (ADR-0024). Sandbox-flavored lookup
	// picks the cross-compiled Linux variant when present so this path works
	// off macOS/Windows hosts without per-user cross-compilation.
	//
	// Baked image (issue #957): the published image carries aileron-mcp at
	// sandboxMCPBinPath, so skip host resolution (a missing host binary is
	// not fatal on a sealed runtime) and skip the host-mount. ConfigureMCP
	// below still targets sandboxMCPBinPath, identical on both branches.
	if sandboxBakedMCPVersion(ctx, plan.Runtime, plan.Image) == "" {
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
	}

	// Build the MCP env the in-container aileron-mcp needs to reach the
	// daemon. Reads from agentEnv so the URL rewrite for the runtime
	// (host.docker.internal) is honored.
	mcpEnv := sandboxMCPEnv(agentEnv)
	extraArgs, mcpMounts, mcpErr := config.Agent.ConfigureMCP(sandboxMCPBinPath, mcpEnv, config.Dir, ModeSandbox)
	if mcpErr != nil {
		return LaunchResult{}, fmt.Errorf("configuring MCP for %s: %w", config.Agent.Name(), mcpErr)
	}
	collapsed, err := collapseNestedMCPMounts(mounts, mcpMounts)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("placing MCP config for %s: %w", config.Agent.Name(), err)
	}
	mounts = collapsed

	// Project the canonical skill store read-only at the agent's skills
	// path (install-once / launch-anywhere, #1508). This runs BEFORE the
	// chown for the same reason the MCP collapse does: when the skills
	// target nests inside an AuthSpec directory mount (Claude's writable
	// /home/agent/.claude/), the store content is copied into that mount's
	// host-side source and must be in place before the tree is re-owned to
	// the foreign agent UID.
	mounts, err = appendSkillsMount(mounts, config.Agent, skillStoreDir())
	if err != nil {
		return LaunchResult{}, fmt.Errorf("projecting skills for %s: %w", config.Agent.Name(), err)
	}

	// Chown the AuthSpec transient tree to the in-container agent UID now
	// that the MCP config has been placed into it (issue #1488). The chown
	// MUST follow the collapse above: the collapse does an unprivileged
	// host-side MkdirAll+WriteFile of Codex's config.toml into the AuthSpec
	// dir mount's host source, and chowning the tree to the foreign agent
	// UID with mode 0700 first would make that write fail with EPERM on
	// rootful Docker Linux. The single delegated chown here re-owns
	// config.toml alongside auth.json. A nil hook is a safe no-op.
	if chownAuthTree != nil {
		if err := chownAuthTree(); err != nil {
			return LaunchResult{}, fmt.Errorf("chowning auth tree for %s: %w", config.Agent.Name(), err)
		}
	}

	command := append([]string{commandName}, config.Agent.Args(ModeSandbox)...)
	command = append(command, extraArgs...)
	command = append(command, config.Args...)
	command, user := sandboxEntrypointCommand(
		command,
		sandboxProxyBootstrapActive(agentEnv),
		workspaceUIDRemapActive(plan.Runtime),
	)

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
	//
	// Cleanup discipline is borrowed from launchDirect (signal.Stop +
	// close + drain) — but NOT its signal-*forwarding* behavior.
	// launchDirect forwards the caught signal to the child
	// (cmd.Process.Signal(sig)); the salvage path does the opposite,
	// catching the signal and issuing a graceful stop itself, so the
	// forward-to-child loop is intentionally not reused here.
	sigCh := make(chan os.Signal, 1)
	sandboxSignalNotify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	salvageDone := make(chan struct{})
	// signalFired records whether the goroutine read a real signal (vs.
	// observing the clean-exit close below). It makes the "the drain
	// below only blocks on salvage when a signal actually fired" intent
	// explicit: on a clean exit the goroutine takes the channel-closed
	// branch and returns immediately, so <-salvageDone is bounded and
	// never waits on Capture.
	var signalFired bool
	go func() {
		defer close(salvageDone)
		sig, ok := <-sigCh
		if !ok {
			// Channel closed by the clean-exit path below; no signal
			// arrived. The container exited on its own and the clean-
			// exit gate owns Capture, so there is nothing to salvage.
			// signalFired stays false; the drain returns at once.
			return
		}
		signalFired = true
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
	// leaks (mirrors launchDirect's signal.Stop discipline — only the
	// cleanup shape transfers, not launchDirect's forward-to-child
	// behavior). The drain is bounded: closing sigCh unblocks the
	// goroutine on the clean-exit path (signalFired stays false) and it
	// returns without running salvage, so <-salvageDone completes at
	// once. Only when a signal actually fired (signalFired true) does the
	// drain wait, and only until the goroutine's stop+Capture finish — so
	// no truncated-mid-PUT race and no hang on the clean path.
	sandboxSignalStop(sigCh)
	close(sigCh)
	<-salvageDone
	// Reading signalFired here is safe: the goroutine's only write to it
	// happens-before it closes salvageDone, which this receive observes.
	if signalFired && sessionLog != nil {
		sessionLog.Info("sandbox salvage completed", "container", containerName)
	}

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

// hostMCPEnv builds the env block aileron-mcp reads when it runs as a
// stdio subprocess of the agent under host launch (ModeHost). It mirrors
// sandboxMCPEnv: the four daemon-coordinate vars are always present, and
// AILERON_TOKEN is included only when the daemon advertises one.
//
// The token is load-bearing for the /v1/actions discovery call, which the
// daemon gates behind Authorization: Bearer (only /v1/health,
// /v1/auth/handshake, and the gateway paths skip auth). Codex spawns its
// MCP server with only the declared config.toml env block, so without an
// explicit AILERON_TOKEN here it 401s and silently falls back to the
// built-in tools (issue #1406). Claude works regardless because Claude
// Code's MCP child inherits the parent process env, but relying on that is
// the bug.
//
// An empty token is omitted entirely rather than injected as an empty
// value: an empty Bearer deterministically 401s, so writing the key would
// only mask the "no token advertised" case as a malformed-token failure.
func hostMCPEnv(daemonURL, sessionID, daemonToken string) map[string]string {
	mcpEnv := map[string]string{
		"AILERON_URL":          daemonURL,
		"AILERON_COMMS_URL":    daemonURL,
		"AILERON_SESSION_ID":   sessionID,
		"AILERON_APPROVAL_URL": daemonURL + "/approvals",
	}
	if daemonToken != "" {
		mcpEnv["AILERON_TOKEN"] = daemonToken
	}
	return mcpEnv
}

// sandboxMCPEnv builds the env block aileron-mcp reads when it runs as
// an in-container stdio subprocess of the agent. Sources values from
// agentEnv so the URL rewrite for the container runtime
// (host.docker.internal on Docker) is preserved and no second
// source-of-truth for the daemon URL is
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

func sandboxRuntimeMounts() ([]sandboxcontainer.Volume, error) {
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
	return mounts, nil
}

// sandboxCacheVolumes maps each declared cache path to a Docker named-volume
// mount. The volume name is derived deterministically from (CLI, identity) so a
// tool's cache survives across launches while a different identity keys a fresh
// store. ContainerPath must be an absolute in-container path; a CLI is required
// because it is part of the volume key. The volumes are mounted read-write
// (caches are writable by definition).
func sandboxCacheVolumes(paths []sandboxcomposition.CachePath) ([]sandboxcontainer.Volume, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	mounts := make([]sandboxcontainer.Volume, 0, len(paths))
	for _, p := range paths {
		cli := strings.TrimSpace(p.CLI)
		if cli == "" {
			return nil, fmt.Errorf("sandbox cache path requires a cli")
		}
		containerPath := strings.TrimSpace(p.ContainerPath)
		if containerPath == "" {
			return nil, fmt.Errorf("sandbox cache path for cli %q requires a container_path", cli)
		}
		if !path.IsAbs(containerPath) {
			return nil, fmt.Errorf("sandbox cache container_path %q for cli %q must be absolute", containerPath, cli)
		}
		mounts = append(mounts, sandboxcontainer.Volume{
			Source: sandboxcomposition.CacheVolumeName(cli, p.Identity),
			Target: containerPath,
			Named:  true,
		})
	}
	return mounts, nil
}

// launchDirect runs the agent with direct stdin/stdout/stderr passthrough.
func launchDirect(cmd *exec.Cmd, config LaunchConfig) (LaunchResult, error) {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return LaunchResult{}, fmt.Errorf("failed to start %s: %w", config.Agent.Name(), err)
	}

	// Host launch FORWARDS the caught signal to the child so the agent's
	// own Ctrl-C handling runs unchanged. This is the opposite of the
	// sandbox salvage path (launchSandbox), which CATCHES the signal and
	// issues a graceful `docker stop` itself. Only launchDirect's cleanup
	// discipline (signal.Stop + close) is reused there, never this
	// forward-to-child loop. See ADR-0025 and issue #999.
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
