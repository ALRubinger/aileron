package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

// containerImageRunner is the production runtime.ImageRunner: it boots the
// verified pinned rung-1/rung-2 image and runs the frozen Flight Plan inside it
// (#1731). It mirrors the sandboxRunContainer seam in internal/launch: the boot
// is a package-level indirection (containerRunFlightPlan) so CLI tests swap in a
// fake image runner and never touch Docker, while production shells out to the
// real container runtime through the same container.Builder path the sandbox
// launch uses.
//
// The image it boots is spec.Image, taken straight from the verified lock by
// the runtime core. containerImageRunner MUST NOT re-resolve or rewrite it: the
// digest is the load-bearing assertion that the environment entered corresponds
// to the lock's signed image. It mounts the frozen store read-only and the
// out-dir writable, then re-enters the aileron binary against the bind-mounted
// unit so the in-container run reaches the same daemon action boundary over the
// existing network wiring.
//
// Daemon-audit reachability (#1759): when daemonEnv is non-nil it injects the
// host daemon coordinates (AILERON_API_URL + AILERON_TOKEN) into the booted
// container's env so the re-entered `aileron skill launch` reaches the SAME host
// daemon action + audit boundary — a record emitted inside the image then
// surfaces in the host `aileron audit list`. Without it the inner binary sees no
// daemon env and resolves/spawns an ephemeral in-container daemon nothing on the
// host can query. A zero-value runner has daemonEnv == nil and injects no env,
// keeping the CLI unit tests deterministic irrespective of any live ~/.aileron
// daemon; production wires the daemon-backed resolver via newLaunchImageRunner.
type containerImageRunner struct {
	// daemonEnv, when non-nil, resolves the daemon env injected into the boot so
	// the in-container launch reaches the host daemon. nil (the zero value) is
	// passthrough (no injected env).
	daemonEnv imageDaemonEnv
	// diag is where the pre-boot CLI-version-skew warning is written. nil (the
	// zero value) means os.Stderr in production; tests set a buffer to assert the
	// warning end-to-end through Run.
	diag io.Writer
}

// diagWriter returns the destination for the skew warning: the injected diag
// writer, or os.Stderr when unset (production wiring leaves diag nil).
func (r containerImageRunner) diagWriter() io.Writer {
	if r.diag != nil {
		return r.diag
	}
	return os.Stderr
}

// envSkillImageBooted marks the image-boot re-entry (#1731). The image runner
// injects it into every whole-plan boot, and runSkillLaunch reads it into
// runtime.Options.InPinnedImage so the in-container re-entry runs the plan
// in-process instead of booting the verified pin a second time (which would
// recurse — the image carries no nested container runtime).
const envSkillImageBooted = "AILERON_SKILL_IMAGE_BOOTED"

// containerRunFlightPlan boots the pinned image and runs the plan inside it. It
// is a package variable purely for symmetry with sandboxRunContainer; the
// production launch swaps the whole runtime.ImageRunner via newLaunchImageRunner
// in tests, so this indirection is the single real-Docker call site.
var containerRunFlightPlan = func(ctx context.Context, runtimeName string, stdout, stderr io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Runner:  sandboxcontainer.DefaultRunner(),
		Stdout:  stdout,
		Stderr:  stderr,
	}.Run(ctx, opts)
}

// containerBakedCLIVersion reads the aileron CLI version baked into the pinned
// image (empty when unlabeled or uninspectable). It is a package variable so the
// CLI tests swap it for a fake and never shell out to `docker image inspect`,
// mirroring the containerRunFlightPlan and sandboxCheckBakedVersionFn seams.
var containerBakedCLIVersion = func(ctx context.Context, runtimeName, image string) string {
	return sandboxcontainer.BakedCLIVersion(ctx, sandboxcontainer.DefaultRunner(), runtimeName, image)
}

// reportBakedCLIVersion surfaces version skew between the pinned rung-1/rung-2
// image's baked aileron CLI and the host CLI that is booting it (#1809). The
// image-boot re-entry runs the baked binary against this host's launch
// (ADR-0027 / #1731), so a mismatch is worth a heads-up but is never fatal: the
// launch always proceeds. A baked value of "" (unlabeled or uninspectable
// image) is silent because a custom image's contract is the operator's
// responsibility under ADR-0027; a match is silent so the stdout of
// `aileron skill launch` stays byte-identical (unlike the sandbox-check
// precedent, this emits no OK line).
func reportBakedCLIVersion(stderr io.Writer, image, baked, host string) {
	if baked == "" || baked == host {
		return
	}
	fmt.Fprintf(stderr, "warning: pinned image %q bakes aileron CLI version %s but the host CLI booting it is version %s. The image-boot re-entry (ADR-0027 / #1731) runs the baked binary against this host's launch; keep both sides on the same release. The launch proceeds.\n", image, baked, host)
}

func (r containerImageRunner) Run(ctx context.Context, spec runtime.ImageRunSpec) (runtime.ImageRunResult, error) {
	if spec.Image == "" {
		return runtime.ImageRunResult{}, fmt.Errorf("skill launch: image runner requires a pinned image")
	}
	runtimeName, err := sandboxcontainer.ResolveRuntime("")
	if err != nil {
		return runtime.ImageRunResult{}, fmt.Errorf("skill launch: resolve container runtime: %w", err)
	}

	// Preflight the CLI-version skew (#1809). The image-boot re-entry runs the
	// image's baked aileron binary against THIS host's launch (ADR-0027 / #1731),
	// so mismatched releases on the two sides are worth a heads-up. This warns on
	// stderr and never fails the launch: an unlabeled/uninspectable image reads as
	// "" and stays silent (custom-image contract is the operator's under ADR-0027),
	// a matching version stays silent (stdout unchanged), and any other value warns.
	reportBakedCLIVersion(r.diagWriter(), spec.Image, containerBakedCLIVersion(ctx, runtimeName, spec.Image), version.Version)

	// Mount the frozen store read-only so the in-container run reads the exact
	// bytes verified on the host, and the out-dir writable so materialized
	// artifacts land where the host expects them.
	const (
		storeMount  = "/aileron/skills"
		outDirMount = "/aileron/out"
	)
	volumes := []sandboxcontainer.Volume{
		{Source: store.New(skillStoreDir).Root(), Target: storeMount, ReadOnly: true},
	}
	if spec.OutDir != "" {
		abs, err := filepath.Abs(spec.OutDir)
		if err != nil {
			return runtime.ImageRunResult{}, fmt.Errorf("skill launch: resolve out-dir: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return runtime.ImageRunResult{}, fmt.Errorf("skill launch: create out-dir: %w", err)
		}
		volumes = append(volumes, sandboxcontainer.Volume{Source: abs, Target: outDirMount})
	}

	// Re-enter the aileron binary against the bind-mounted unit. The in-container
	// run loads the same frozen version from the mounted store and materializes
	// into the mounted out-dir. --store-dir points the inner binary at the
	// bind-mounted store so it reads the exact verified bytes rather than the
	// empty default store inside the image. The daemon action boundary stays
	// reachable over the existing network wiring; no credentials cross this
	// boundary.
	command := []string{"aileron", "skill", "launch", spec.Name, "--store-dir", storeMount}
	if spec.Version != "" {
		command = append(command, "--version", spec.Version)
	}
	if spec.OutDir != "" {
		command = append(command, "--out-dir", outDirMount)
	}

	opts := sandboxcontainer.RunOptions{
		Runtime: runtimeName,
		Image:   spec.Image,
		Volumes: volumes,
		Command: command,
		Name:    flightPlanContainerName(spec),
		// The sentinel tells the in-container re-entry it is ALREADY inside
		// the booted pin: it must run the plan in-process rather than boot
		// the pin again (which would recurse — the image carries no nested
		// container runtime, and with one it would never terminate).
		Env: map[string]string{envSkillImageBooted: "1"},
	}

	// Daemon-audit reachability (#1759): inject the host daemon coordinates so the
	// re-entered in-container launch posts its actions + audit records to the SAME
	// host daemon rather than an ephemeral in-container one. The resolver is
	// spawn-free and gates on config presence: when no daemon URL/token resolves
	// it returns ok=false and the boot carries no injected env, keeping a
	// no-daemon launch working. --add-host host.docker.internal:host-gateway is
	// inherited unconditionally from the shared Builder.Run path on docker+linux
	// (runtime.go runArgs), so the rewritten host is reachable without any new
	// emission here.
	if r.daemonEnv != nil {
		if env, ok := r.daemonEnv.Env(runtimeName); ok {
			if opts.Env == nil {
				opts.Env = env
			} else {
				for k, v := range env {
					opts.Env[k] = v
				}
			}
		}
	}

	if _, err := containerRunFlightPlan(ctx, runtimeName, os.Stdout, os.Stderr, opts); err != nil {
		return runtime.ImageRunResult{}, fmt.Errorf("skill launch: run pinned image %q: %w", spec.Image, err)
	}

	// v1 assembles a minimal result: the resolved inputs are echoed and
	// artifacts are collected from the out-dir mount. The content hash is left
	// to the in-container run's own audit; the CLI surfaces the launch line
	// regardless. Later sub-issues thread the full RunResult back from the
	// in-container run.
	return runtime.ImageRunResult{
		ResolvedInputs: map[string]any(spec.Inputs),
	}, nil
}

// containerToolImageRunner is the production runtime.ToolImageRunner: it
// dispatches a single rung-3 step to its pinned sibling tool image with
// mount → run → collect I/O (#1733). Unlike containerImageRunner (which boots
// one image and runs the WHOLE plan inside it), this boots the tool image for a
// single step: it writes the resolved step input into a host mount dir, mounts
// it read-only at the declared MountPath, mounts a writable collect dir at the
// declared CollectPath's parent, runs the tool, then reads back the collected
// file as the step's output. The real-Docker call goes through the same
// containerRunToolImage indirection so CLI tests swap a fake and never touch
// Docker.
//
// Credential injection (#1769): when proxy is non-nil the runner routes the
// tool container's HTTPS egress through the ADR-0019 daemon forward proxy via
// env-based CA trust, so a matched host binding injects the operator's
// vault-bound credential at the boundary. No credential BYTES cross this
// boundary in the image, env, mounts, or args: only a read-only CA and
// placeholder (non-secret) creds are set. A zero-value runner has proxy == nil
// and stays passthrough (no CA mount, no env), which keeps the CLI unit tests
// deterministic irrespective of any live ~/.aileron daemon.
type containerToolImageRunner struct {
	// proxy, when non-nil, enriches the dispatch with proxy egress + CA trust.
	// nil (the zero value) is passthrough.
	proxy toolProxyBootstrapper
}

// containerToolInputFile is the filename the resolved step input is written to
// inside the mount dir. The tool reads its input from MountPath/<this>.
const containerToolInputFile = "input.json"

// containerRunToolImage boots the pinned tool image and runs it. It is a package
// variable so CLI tests swap the single real-Docker call site with a recorder,
// mirroring containerRunFlightPlan.
var containerRunToolImage = func(ctx context.Context, runtimeName string, stdout, stderr io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Runner:  sandboxcontainer.DefaultRunner(),
		Stdout:  stdout,
		Stderr:  stderr,
	}.Run(ctx, opts)
}

func (r containerToolImageRunner) Run(ctx context.Context, spec runtime.ToolRunSpec) (runtime.ToolRunResult, error) {
	if spec.Image == "" {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: tool image runner requires a pinned image")
	}
	runtimeName, err := sandboxcontainer.ResolveRuntime("")
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: resolve container runtime: %w", err)
	}

	// A per-dispatch scratch root holds the mount input and the collect output.
	// It is removed after the run so a step's I/O never leaks between dispatches.
	scratch, err := os.MkdirTemp("", "aileron-tool-"+spec.StepID+"-")
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: create tool scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)

	var volumes []sandboxcontainer.Volume

	// Mount side: serialize the resolved input to a host file and bind-mount its
	// directory read-only at the declared MountPath. The input is binding-resolved
	// data only, never a credential.
	if spec.MountPath != "" {
		mountHost := filepath.Join(scratch, "mount")
		if err := os.MkdirAll(mountHost, 0o755); err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: create tool mount dir: %w", err)
		}
		payload, err := json.Marshal(spec.Input)
		if err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: serialize tool input: %w", err)
		}
		if err := os.WriteFile(filepath.Join(mountHost, containerToolInputFile), payload, 0o644); err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: write tool input: %w", err)
		}
		volumes = append(volumes, sandboxcontainer.Volume{Source: mountHost, Target: spec.MountPath, ReadOnly: true})
	}

	// Collect side: bind-mount a writable host dir at the declared CollectPath's
	// parent so the tool can write its output there, and read the file back after
	// the run.
	var collectHost, collectFile string
	if spec.CollectPath != "" {
		collectHost = filepath.Join(scratch, "collect")
		if err := os.MkdirAll(collectHost, 0o755); err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: create tool collect dir: %w", err)
		}
		// spec.CollectPath is a container-internal (Linux) path, so parse it with
		// path, not filepath: on Windows filepath.Dir would yield backslashes
		// (\work\out) and produce an invalid, non-matching container mount target.
		collectFile = path.Base(spec.CollectPath)
		volumes = append(volumes, sandboxcontainer.Volume{Source: collectHost, Target: path.Dir(spec.CollectPath)})
	}

	opts := sandboxcontainer.RunOptions{
		Runtime: runtimeName,
		Image:   spec.Image,
		Volumes: volumes,
		Name:    toolContainerName(spec),
		// ToolDispatch selects the rung-3 sealed-tool contract in the shared
		// container builder: the image's baked entrypoint runs with no command,
		// and no implicit operator-CWD workspace mount is emitted — the tool
		// sees exactly the declared mounts above and nothing else.
		ToolDispatch: true,
	}

	// Credential-injection enrichment (#1769): when a proxy bootstrapper is
	// wired, route the tool container's HTTPS egress through the ADR-0019 daemon
	// forward proxy via env-based CA trust so a matched host binding injects the
	// operator's vault-bound credential at the boundary. Prepare fails closed: a
	// dispatch that resolved daemon config but could not write its session CA
	// must NOT silently egress un-proxied. The cleanup is deferred so the
	// session CA is removed even if the boot below fails. RunOptions.Command is
	// never set — the tool image's own entrypoint must run.
	if r.proxy != nil {
		bootstrap, cleanup, ok, err := r.proxy.Prepare(runtimeName, spec.StepID)
		if err != nil {
			// Prepare may have written a partial session CA before failing; run
			// its cleanup so nothing leaks on the fail-closed path.
			if cleanup != nil {
				cleanup()
			}
			return runtime.ToolRunResult{}, err
		}
		if ok {
			defer cleanup()
			opts.Volumes = append(opts.Volumes, bootstrap.Mount)
			opts.Env = bootstrap.Env
		}
	}

	if _, err := containerRunToolImage(ctx, runtimeName, os.Stdout, os.Stderr, opts); err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: run pinned tool image %q: %w", spec.Image, err)
	}

	// Read back the collected output when a collect path was declared. A step
	// with no collect produces no output.
	if collectHost == "" {
		return runtime.ToolRunResult{}, nil
	}
	// The collect dir is bind-mounted writable and the tool controls its
	// contents. A compromised or malicious tool image could write the collected
	// file as a SYMLINK pointing at an arbitrary host path (e.g. /etc/passwd)
	// readable by the aileron process; a naive os.ReadFile would follow it and
	// smuggle host file contents into the step output (CWE-61), defeating the
	// isolation this per-step dispatch exists to provide. Lstat first and refuse
	// anything that is not a regular file, so only bytes the tool actually wrote
	// into the mount are collected.
	collectAbs := filepath.Join(collectHost, collectFile)
	info, err := os.Lstat(collectAbs)
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: collect output from %q: %w", spec.CollectPath, err)
	}
	if !info.Mode().IsRegular() {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: collected output %q is not a regular file (refusing to follow symlinks or read special files)", spec.CollectPath)
	}
	out, err := os.ReadFile(collectAbs)
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: collect output from %q: %w", spec.CollectPath, err)
	}
	return runtime.ToolRunResult{Output: string(out)}, nil
}

// toolContainerName builds an addressable, run-unique name for a per-step tool
// dispatch, so two concurrent dispatches never collide on the container name.
func toolContainerName(spec runtime.ToolRunSpec) string {
	id := spec.StepID
	if id == "" {
		id = "step"
	}
	return "aileron-flightplan-tool-" + id + "-" + randomSuffix()
}

// flightPlanContainerName builds an addressable container name for the boot. The
// name keeps a stable, human-readable base (the skill name and version) and
// appends a short random suffix so two concurrent launches of the same frozen
// unit never collide on the container name. A stop/cleanup path reuses the value
// returned here for a given launch rather than recomputing it.
func flightPlanContainerName(spec runtime.ImageRunSpec) string {
	v := spec.Version
	if v == "" {
		v = "latest"
	}
	return "aileron-flightplan-" + spec.Name + "-" + v + "-" + randomSuffix()
}

// randomSuffix returns a short hex token for disambiguating container names. On
// the vanishingly unlikely RNG failure it degrades to a fixed token rather than
// failing the launch, since the suffix is a collision-avoidance convenience, not
// a security boundary.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}
