package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

// containerImageRunner is the production runtime.ImageRunner: it boots the
// verified pinned environment image and runs the frozen Flight Plan inside it
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
// daemon action + audit boundary, a record emitted inside the image then
// surfaces in the host `aileron audit list`. Without it the inner binary sees no
// daemon env and resolves/spawns an ephemeral in-container daemon nothing on the
// host can query. A zero-value runner has daemonEnv == nil and injects no env,
// keeping the CLI unit tests deterministic irrespective of any live ~/.aileron
// daemon; production wires the daemon-backed resolver via newLaunchImageRunner.
//
// Boot-time credential injection (#1828): when proxy is non-nil the runner
// routes the whole booted plan container's HTTPS egress through the ADR-0019
// daemon forward proxy via env-based CA trust, so a matched host binding injects
// the operator's vault-bound credential at the boundary. No credential BYTES
// cross this boundary in the image, env, mounts, or args: only a read-only CA
// and the catalog-derived placeholder (non-secret) creds are set. A zero-value
// runner has proxy == nil and stays passthrough.
type containerImageRunner struct {
	// daemonEnv, when non-nil, resolves the daemon env injected into the boot so
	// the in-container launch reaches the host daemon. nil (the zero value) is
	// passthrough (no injected env).
	daemonEnv imageDaemonEnv
	// proxy, when non-nil, enriches the boot with proxy egress + CA trust +
	// catalog-derived placeholder creds. nil (the zero value) is passthrough.
	proxy planProxyBootstrapper
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
// recurse; the image carries no nested container runtime).
const envSkillImageBooted = "AILERON_SKILL_IMAGE_BOOTED"

// reservedBootEnvKeys are the authoritative boot-env keys that
// containerImageRunner.Run sets ITSELF on opts.Env before merging the
// proxy/placeholder bootstrap env: the host daemon coordinates (#1759) and the
// image-boot sentinel. The bootstrap env is a union of the ADR-0019 proxy/CA
// keys and the image-declared credential placeholders (from the pin's
// devcontainer.metadata label). PlaceholderEnv only rejects convention-vs-
// convention collisions and the proxy overlay only shadows its own keys, so a
// placeholder that (mis)declares one of these reserved keys would otherwise
// reach the merge below and clobber the authoritative daemon token/URL or the
// boot sentinel silently. Guard the merge: reject any bootstrap key that is a
// reserved boot key, failing closed with a loud, variable-naming error rather
// than booting with a poisoned token/URL or a lost re-entry sentinel.
var reservedBootEnvKeys = map[string]struct{}{
	"AILERON_TOKEN":     {},
	"AILERON_API_URL":   {},
	envSkillImageBooted: {},
}

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

// reportBakedCLIVersion surfaces version skew between the pinned environment
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
	// Re-emit the launch input overrides as --input name=value on the in-container
	// re-entry (#1802). Without this the inner binary sees no overrides and every
	// input silently resolves to its default, while the runner still echoes
	// spec.Inputs as ResolvedInputs, falsely telling the operator the overrides
	// applied. CLI-path values arrive as strings via inputFlag.Set, and the inner
	// re-entry parses them with the same inputFlag, so the string round-trip is
	// exact. Keys are appended in sorted order so the emitted command is
	// deterministic. A non-string value in the map[string]any seam type has no
	// exact name=value round-trip, so refuse the boot with an explicit error
	// rather than coerce or drop it (the issue's never-silently-drop requirement).
	if len(spec.Inputs) > 0 {
		keys := make([]string, 0, len(spec.Inputs))
		for k := range spec.Inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s, ok := spec.Inputs[k].(string)
			if !ok {
				return runtime.ImageRunResult{}, fmt.Errorf("skill launch: image-boot re-entry cannot pass non-string override for input %q (got %T): refusing to boot rather than drop or coerce it", k, spec.Inputs[k])
			}
			command = append(command, "--input", k+"="+s)
		}
	}

	opts := sandboxcontainer.RunOptions{
		Runtime: runtimeName,
		Image:   spec.Image,
		Volumes: volumes,
		Command: command,
		Name:    flightPlanContainerName(spec),
		// The sentinel tells the in-container re-entry it is ALREADY inside
		// the booted pin: it must run the plan in-process rather than boot
		// the pin again (which would recurse; the image carries no nested
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

	// Boot-time credential injection (#1828): when a proxy bootstrapper is wired,
	// route the whole booted plan container's HTTPS egress through the ADR-0019
	// daemon forward proxy via env-based CA trust so a matched host binding
	// injects the operator's vault-bound credential at the boundary. Prepare
	// fails closed: a boot that resolved daemon config but could not write its
	// session CA (or whose declared conventions were malformed) must NOT silently
	// egress un-proxied; it runs cleanup and fails the boot. The proxy env and
	// the sentinel + daemon env are expected to be disjoint key sets
	// (HTTPS_PROXY/CA/placeholders vs
	// AILERON_SKILL_IMAGE_BOOTED/AILERON_API_URL/AILERON_TOKEN); the
	// reservedBootEnvKeys guard below ENFORCES that invariant, failing the boot if
	// any bootstrap/placeholder key collides with a reserved boot key rather than
	// letting the merge clobber the sentinel or daemon coordinates. Cleanup is
	// deferred so the session CA is removed after the (synchronous) run completes.
	if r.proxy != nil {
		bootstrap, cleanup, ok, err := r.proxy.Prepare(ctx, runtimeName, spec.Image, spec.Name)
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			return runtime.ImageRunResult{}, err
		}
		if ok {
			if cleanup != nil {
				defer cleanup()
			}
			for k := range bootstrap.Env {
				if _, reserved := reservedBootEnvKeys[k]; reserved {
					return runtime.ImageRunResult{}, fmt.Errorf("skill launch: bootstrap/placeholder env declares reserved boot key %q: refusing to boot rather than clobber the authoritative daemon coordinates or the image-boot sentinel", k)
				}
			}
			opts.Volumes = append(opts.Volumes, bootstrap.Mount)
			if opts.Env == nil {
				opts.Env = bootstrap.Env
			} else {
				for k, v := range bootstrap.Env {
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
