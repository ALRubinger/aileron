package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/internal/cli/progress"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"
	"github.com/ALRubinger/aileron/internal/sandbox/container"
	sandboxtoolchain "github.com/ALRubinger/aileron/internal/sandbox/toolchain"
	cliversion "github.com/ALRubinger/aileron/internal/version"
)

// newDigestResolver and newFeatureComposer are package-level seams so CLI
// tests exercise the freeze orchestration without a container runtime. The
// production implementations wire container.DefaultRunner() (base-image
// inspect) and container.Builder (environment-tools Feature composition).
var newDigestResolver = func() freeze.DigestResolver { return runtimeDigestResolver{} }
var newFeatureComposer = func() freeze.FeatureComposer { return builderFeatureComposer{} }

// freezeProgress carries a factory that mints a fresh progress.Indicator for a
// single bracketed step in the in-flight freeze run. It exists because a
// progress.Indicator is single-shot (the first Done/Fail marks it finished and
// inert), so pull, the multi-arch build, and the daemon-load build each need
// their own indicator over the same destination and quiet setting. The
// production newImageInspector (a no-arg package var reached from inside the
// stateless runtimeDigestResolver / builderFeatureComposer) reads this factory
// and installs it on the inspector it builds. runSkillFreeze sets it before
// freeze.Run and resets it on return. It is nil outside a run and in tests that
// do not opt in, so every inspector method nil-guards it and stays a no-op.
// This keeps runtimeDigestResolver{} / builderFeatureComposer{} zero-value
// constructible and leaves the runtime-free freeze core untouched.
var freezeProgress func() *progress.Indicator

func runSkillFreeze(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill freeze", flag.ContinueOnError)
	flags.SetOutput(stderr)
	signingKey := flags.String("signing-key", "", "Path to the PEM ed25519 signing key (falls back to $"+freeze.SigningKeyEnv+")")
	version := flags.String("version", "", "Semver label recorded in the lock (for example 1.0.0)")
	publisher := flags.String("publisher", "", "Publisher authority to attribute the plan to (github://owner/repo or github://owner). When set, launch enforces publisher trust against the keyring.")
	quiet := flags.Bool("quiet", false, "Suppress the live build and pull progress feedback (the freeze summary still prints)")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	target := positionals[0]

	// A publisher is optional (#1900), but omitting it means the frozen plan
	// carries no launch-time publisher-trust gate: any locally-signed plan
	// launches. Warn (never silently succeed) so the author knows the plan is
	// self-attesting only. When set, validate the authority parses as a
	// connector-style FQN before freezing so a malformed value fails fast
	// rather than sealing an unusable publisher into the signed lock.
	if *publisher == "" {
		fmt.Fprintln(stderr, "warning: freezing without --publisher; the frozen plan carries no publisher-trust gate and any locally-signed copy will launch")
	} else if perr := validatePublisherAuthority(*publisher); perr != nil {
		fmt.Fprintf(stderr, "error: invalid --publisher %q: %v\n", *publisher, perr)
		return 1
	}

	raw, err := readSkillForFreeze(target)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// Install a progress-indicator factory over stdout for the whole run. Each
	// bracketed step (pull, multi-arch build, daemon-load build) mints its own
	// indicator because an indicator is single-shot. TTY autodetection fires only
	// when stdout is an *os.File attached to a terminal (animated spinner); a
	// captured bytes.Buffer is non-*os.File, so it degrades to plain,
	// control-character-free lines with no force flag. --quiet suppresses output
	// on both paths. The factory is threaded to the pull/build sites through
	// freezeProgress so the runtime-free freeze core and the zero-value
	// resolver/composer stay untouched.
	prev := freezeProgress
	freezeProgress = func() *progress.Indicator {
		return progress.New(stdout, progress.WithQuiet(*quiet))
	}
	defer func() { freezeProgress = prev }()

	res, err := freeze.Run(context.Background(), raw, freeze.Options{
		Version:        *version,
		Publisher:      *publisher,
		CLIVersion:     cliversion.Version,
		SigningKeyPath: *signingKey,
		Resolver:       newDigestResolver(),
		Composer:       newFeatureComposer(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// Persist the frozen version into the canonical store. The version
	// directory id is the short contentHash so distinct content lands in
	// distinct, immutable directories.
	s := store.New(skillStoreDir)
	id := frozenVersionID(res.ContentHash)
	if err := s.WriteFrozen(res.Name, store.FrozenVersion{
		ID:        id,
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Froze skill %q\n", res.Name)
	if res.Version != "" {
		fmt.Fprintf(stdout, "  Version:     %s\n", res.Version)
	}
	fmt.Fprintf(stdout, "  ContentHash: %s\n", res.ContentHash)
	fmt.Fprintf(stdout, "  Stored at:   %s\n", s.FrozenDir(res.Name, id))
	return 0
}

// validatePublisherAuthority checks that a --publisher value is a connector-
// style authority freeze can seal into the lock (#1900). Both shapes the launch
// gate resolves are accepted: a full per-repo FQN (`github://owner/repo`, via
// cstore.ParseFQN) and a bare owner (`github://owner`, which ParseFQN rejects
// as "missing repo segment" but the keyring's owner grants are keyed on). This
// mirrors the input forms `aileron keyring trust` accepts, so a publisher
// trusted there matches a publisher sealed here.
func validatePublisherAuthority(authority string) error {
	if isBareOwnerAuthority(authority) {
		return nil
	}
	if _, err := cstore.ParseFQN(authority); err != nil {
		return err
	}
	return nil
}

// readSkillForFreeze loads the SKILL.md bytes to freeze. The target is
// either an installed skill name (read from the store) or a filesystem path
// to a skill directory or a SKILL.md file.
func readSkillForFreeze(target string) ([]byte, error) {
	// A filesystem path (directory or SKILL.md) takes precedence when it
	// exists, so a local author can freeze before installing.
	if info, statErr := os.Stat(target); statErr == nil {
		path := target
		if info.IsDir() {
			path = filepath.Join(target, "SKILL.md")
		} else if filepath.Base(target) != "SKILL.md" {
			return nil, fmt.Errorf("freeze source %q is not a SKILL.md", target)
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		return b, nil
	}

	// Otherwise treat it as an installed skill name.
	s := store.New(skillStoreDir)
	b, readErr := s.Read(target)
	if readErr != nil {
		return nil, fmt.Errorf("no installed skill or path %q (install it first, or pass a path to a SKILL.md): %w", target, readErr)
	}
	return b, nil
}

// frozenVersionID derives the immutable store directory id from a
// contentHash. It strips the `sha256:` prefix and takes a short, stable
// slug so the directory name is filesystem-friendly while still being
// collision-resistant for distinct content.
func frozenVersionID(contentHash string) string {
	h := strings.TrimPrefix(contentHash, "sha256:")
	if len(h) > 16 {
		h = h[:16]
	}
	return h
}

// imageInspector drives the small set of container-runtime commands the
// freeze resolvers need (pull, image inspect). It is the seam over
// container.Runner + the resolved runtime name so the pull-then-inspect and
// RepoDigests-then-Id fallback logic is unit-testable without Docker.
type imageInspector struct {
	runner  container.Runner
	runtime string
	// newProgress mints a fresh single-shot progress indicator for one bracketed
	// step. The production newImageInspector installs freezeProgress here, so the
	// pull and both build sites can each bracket their blocking step with liveness
	// feedback. It is nil in tests that do not opt in and in the zero-value
	// inspector, so every call site nil-guards it.
	newProgress func() *progress.Indicator
}

// newImageInspector builds an inspector over the real container runtime,
// resolving the runtime name (erroring when none is on PATH). It is a
// package-level seam so CLI tests inject a fake-runner inspector and the
// resolver/composer delegation runs without Docker.
var newImageInspector = func() (imageInspector, error) {
	runtimeName, err := container.ResolveRuntime(container.DefaultRuntime)
	if err != nil {
		return imageInspector{}, fmt.Errorf("resolve container runtime: %w", err)
	}
	return imageInspector{runner: container.DefaultRunner(), runtime: runtimeName, newProgress: freezeProgress}, nil
}

// pull best-effort pulls ref. A failure is intentionally non-fatal: the
// image may already be local, in which case the inspect still yields a
// digest.
func (in imageInspector) pull(ctx context.Context, ref string) {
	var ind *progress.Indicator
	if in.newProgress != nil {
		ind = in.newProgress()
		ind.Start("Pulling base image")
	}
	_ = in.runner.Run(ctx, in.runtime, []string{"pull", ref}, io.Discard, io.Discard)
	// Resolve to done even on a pull error: the pull is best-effort and the
	// image may already be local, so inspect still proceeds. A Fail here would
	// mislead, since the freeze is not failing at this step.
	if ind != nil {
		ind.Done("Pulled base image")
	}
}

// inspectFormat runs `image inspect --format <format> <image>` and returns
// its stdout.
func (in imageInspector) inspectFormat(ctx context.Context, image, format string) ([]byte, error) {
	var out bytes.Buffer
	if err := in.runner.Run(ctx, in.runtime, []string{"image", "inspect", "--format", format, image}, &out, io.Discard); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// runtimeDigestResolver resolves a base-image reference to its digest by
// pulling (best-effort) then inspecting RepoDigests through the shared
// container Runner seam. It pins by digest, never by tag.
type runtimeDigestResolver struct{}

func (runtimeDigestResolver) ResolveDigest(ctx context.Context, ref string) (string, error) {
	in, err := newImageInspector()
	if err != nil {
		return "", err
	}
	return in.resolveDigest(ctx, ref)
}

// resolveDigest is the seam-driven core of ResolveDigest.
func (in imageInspector) resolveDigest(ctx context.Context, ref string) (string, error) {
	in.pull(ctx, ref)
	out, err := in.inspectFormat(ctx, ref, "{{json .RepoDigests}}")
	if err != nil {
		return "", fmt.Errorf("inspect image %q: %w", ref, err)
	}
	return digestFromRepoDigests(out, ref)
}

// digestFromRepoDigests extracts the `sha256:` digest from a docker
// `RepoDigests` JSON array (for example
// `["registry.example.com/runner@sha256:abc..."]`). When several entries
// are present (the same image known under multiple repositories), it returns
// the digest of the entry whose repository matches the requested ref, so
// freeze never pins a digest for the wrong repository. It falls back to the
// sole entry when there is exactly one. A local-only image with no
// RepoDigests yields a clear error so freeze never silently pins nothing.
func digestFromRepoDigests(jsonArr []byte, ref string) (string, error) {
	var repoDigests []string
	if err := json.Unmarshal(bytes.TrimSpace(jsonArr), &repoDigests); err != nil {
		return "", fmt.Errorf("parse RepoDigests for %q: %w", ref, err)
	}
	wantRepo := repoOfRef(ref)
	var soleDigest string
	matched := 0
	for _, rd := range repoDigests {
		i := strings.LastIndex(rd, "@")
		if i < 0 {
			continue
		}
		repo, digest := rd[:i], rd[i+1:]
		soleDigest = digest
		matched++
		if repoOfRef(repo) == wantRepo {
			return digest, nil
		}
	}
	if matched == 1 {
		// A single RepoDigests entry under a different-looking repo string is
		// still the right image (for example a registry-host normalization);
		// pin it rather than failing.
		return soleDigest, nil
	}
	if matched > 1 {
		return "", fmt.Errorf("image %q has multiple registry digests and none match its repository; push a single-repository tag so freeze can pin it unambiguously", ref)
	}
	return "", fmt.Errorf("image %q has no registry digest (RepoDigests empty); push it to a registry so freeze can pin it by digest", ref)
}

// repoOfRef returns the repository portion of an image reference, stripping
// any `:tag` or `@digest` suffix. A `:` that is part of a registry host port
// (for example `registry.example.com:5000/runner`) is preserved because it
// precedes the first `/`.
func repoOfRef(ref string) string {
	// Strip a digest suffix first.
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// Strip a tag suffix: the last `:` that comes after the last `/`.
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	return ref
}

// builderFeatureComposer composes catalog-resolved Feature references onto the
// digest-pinned base image through the container Builder (the #1454 build
// pipeline) for every supported architecture and returns the per-arch set of
// serialization-agnostic config content digests. It routes the environment-tools
// composition through the same Builder / composition path the sandbox uses
// (composition.ToolsPlan's synthesized devcontainer), not a bespoke build
// (issue #2036).
type builderFeatureComposer struct{}

// freezeOCILayoutDir returns the deterministic directory a multi-arch composed
// build writes its OCI image layout to, keyed by the composed image's local tag
// (never an ephemeral temp dir), so the layout is a stable artifact the S4
// publish path can consume. It delegates to composition.OCILayoutDir so freeze
// (write) and publish (read) map a tag to the same path from one source; it stays
// a package var so freeze tests redirect it at a pre-staged synthetic layout.
var freezeOCILayoutDir = composition.OCILayoutDir

// readOCILayoutConfigDigests is the seam over
// ociremote.ConfigContentDigestsFromOCILayout so composer tests drive the
// per-arch mapping and build orchestration without staging a real OCI layout on
// disk; the layout read itself is covered in internal/flightplan/ociremote.
var readOCILayoutConfigDigests = ociremote.ConfigContentDigestsFromOCILayout

func (builderFeatureComposer) ComposeDigest(ctx context.Context, base string, features []string) ([]freeze.PlatformDigest, error) {
	in, err := newImageInspector()
	if err != nil {
		return nil, err
	}
	// Preflight the multi-arch toolchain BEFORE building. On a miss (no buildx, or
	// the QEMU emulators are not registered) abort with an actionable remediation
	// rather than silently producing a single-arch pin (Q2). No pin is emitted.
	if err := container.CheckMultiArchBuild(ctx, in.runner, in.runtime); err != nil {
		return nil, err
	}
	// Resolve the devcontainer toolchain from flag/env/default, mirroring the
	// `aileron sandbox` and launch callers (freeze has no --toolchain flag, so
	// the flag args are empty and resolution is env → default(managed)). Without
	// this the Builder sees an empty ToolchainMode (normalized to managed) with
	// no provisioner and hard-errors, so freeze-with-tools never composes.
	toolchainMode, nodeBinary, cliEntrypoint := container.ResolveToolchainSelection("", "", "", os.Getenv)
	b := container.Builder{
		Runtime: in.runtime,
		Runner:  in.runner,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	}
	// setBuildSinks points the Builder's stdout/stderr at the buildkit-progress
	// sink for the given indicator (falling back to io.Discard when no indicator
	// is active). The sink parses the BUILDKIT_PROGRESS=rawjson stream buildx emits
	// on stderr into a determinate percentage, and degrades to indeterminate
	// liveness on any non-rawjson output, so it never regresses today's behavior.
	// The stderr sink stays the OUTER argument of runBuildStep's
	// io.MultiWriter(stderr, &buf) tee, so the daemon-unreachable capture buffer
	// still sees every byte and the sink never consumes the stream. It emits no
	// bytes itself, so the non-TTY/quiet plain-line contract holds.
	setBuildSinks := func(ind *progress.Indicator) {
		if ind == nil {
			b.Stdout = io.Discard
			b.Stderr = io.Discard
			return
		}
		b.Stdout = progress.NewBuildkitProgressWriter(ind)
		b.Stderr = progress.NewBuildkitProgressWriter(ind)
	}
	// Wire the managed provisioner only on the managed branch; the host-npx
	// opt-out must never carry a provisioner (its no-network/no-provision
	// contract), matching the sandbox and launch callers.
	if container.IsManagedToolchain(toolchainMode) {
		b.Provisioner = sandboxtoolchain.Provisioner{}
	}

	// The composed image's local tag is byte-identical to the LocalTag the freeze
	// core records for the pin (both derive from the same base + feature refs), so
	// the layout path is stable and the daemon-loaded image is addressable by it.
	localTag := composition.LocalToolsImageTag(base, features)
	dest, err := freezeOCILayoutDir(localTag)
	if err != nil {
		return nil, err
	}
	// The layout dir is a stable, tag-keyed path reused across freezes of the same
	// composition, so clear any prior contents before the build: a crashed or
	// interrupted earlier run could leave a partial index.json or stale blobs that
	// the per-arch read would otherwise pick up. buildx writes a fresh layout into
	// the emptied dir.
	if err := os.RemoveAll(dest); err != nil {
		return nil, fmt.Errorf("clear freeze OCI layout dir %q: %w", dest, err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("prepare freeze OCI layout dir %q: %w", dest, err)
	}

	// Build the composed image for both linux/amd64 and linux/arm64 to a persisted
	// OCI image layout (no daemon load), then read the per-arch config content
	// digests straight from that layout — no registry push, no registry auth.
	buildOpts := container.BuildOptions{
		Plan:                      composition.ToolsPlan(base, features),
		Tag:                       localTag,
		Policy:                    container.BuildPolicyAlways,
		ToolchainMode:             toolchainMode,
		NodeBinary:                nodeBinary,
		DevcontainerCLIEntrypoint: cliEntrypoint,
		Platforms:                 composition.MultiArchPlatforms,
		OCILayoutDest:             dest,
		// Scope the multi-arch build to the dedicated docker-container builder the
		// preflight provisioned (CheckMultiArchBuild). The single-arch daemon-load
		// build below intentionally omits this so it stays on the default `docker`
		// driver and lands the image in the local daemon (issue #2054).
		BuildxBuilder: container.FreezeBuilderName,
		// Request structured buildkit progress so the build sink can render a
		// determinate percentage; it carries into loadOpts below too, so both build
		// steps get it (issue #2084).
		ProgressRawJSON: true,
	}
	var buildInd *progress.Indicator
	if in.newProgress != nil {
		buildInd = in.newProgress()
		buildInd.Start("Building environment image")
	}
	setBuildSinks(buildInd)
	result, err := b.Build(ctx, buildOpts)
	if err != nil {
		if buildInd != nil {
			buildInd.Fail("Building environment image")
		}
		return nil, fmt.Errorf("compose environment tools (multi-arch): %w", err)
	}
	if buildInd != nil {
		buildInd.Done("Built environment image")
	}
	perArch, err := readOCILayoutConfigDigests(ctx, result.OCILayoutDir)
	if err != nil {
		return nil, fmt.Errorf("read composed per-arch config digests from %q: %w", result.OCILayoutDir, err)
	}

	// The multi-arch OCI-layout build does not load into the daemon. Run the same
	// composed build once more for the host architecture with a daemon load under
	// LocalTag; buildx reuses the layer cache, so it is cheap. This host-arch daemon
	// image is intentionally retained: local `launch` of an un-published Flight Plan
	// resolves the composed image straight from the daemon under LocalTag, with no
	// publish step in between. Publish itself no longer needs it: as of S4 (#2047)
	// it consumes the OCI layout directly. Dropping this second build is a documented
	// launch-side follow-up, not done here.
	loadOpts := buildOpts
	loadOpts.Platforms = nil
	loadOpts.OCILayoutDest = ""
	// loadOpts inherits ProgressRawJSON=true from buildOpts (only Platforms and
	// OCILayoutDest are zeroed), so this single-arch daemon-load build also emits
	// determinate buildkit progress; BuildxBuilder is inert here because Platforms
	// is nil, so the build stays on the default `docker` driver (issue #2084).
	var loadInd *progress.Indicator
	if in.newProgress != nil {
		loadInd = in.newProgress()
		loadInd.Start("Loading image into local daemon")
	}
	setBuildSinks(loadInd)
	if _, err := b.Build(ctx, loadOpts); err != nil {
		if loadInd != nil {
			loadInd.Fail("Loading image into local daemon")
		}
		return nil, fmt.Errorf("load composed image into the local daemon under %q: %w", localTag, err)
	}
	if loadInd != nil {
		loadInd.Done("Loaded image into local daemon")
	}

	out := make([]freeze.PlatformDigest, 0, len(perArch))
	for _, p := range perArch {
		out = append(out, freeze.PlatformDigest{OS: p.OS, Arch: p.Arch, Digest: p.Digest})
	}
	return out, nil
}

// localImageContentDigest resolves a local image tag to its serialization-
// agnostic config content digest (see internal/flightplan/imgconfig): a hash
// over the parsed config's execution-relevant fields, not the config blob's own
// sha256. The composed-tools freeze producer now reads the per-arch content
// digests from the built OCI layout, but the launch/boot path still resolves the
// host-daemon image under LocalTag to the same content digest so the boot-time
// digest compare is apples-to-apples with the recorded per-arch pin. Binding to
// the content digest (rather than the image Id) is what makes the check survive
// the containerd store's push-time config re-serialization (issue #2014).
func (in imageInspector) localImageContentDigest(ctx context.Context, image string) (string, error) {
	raw, err := in.inspectFormat(ctx, image, "{{json .}}")
	if err != nil {
		return "", fmt.Errorf("resolve config content digest for image %q: %w", image, err)
	}
	cc, err := imgconfig.FromDockerInspect(raw)
	if err != nil {
		return "", fmt.Errorf("resolve config content digest for image %q: %w", image, err)
	}
	return cc.ContentDigest()
}
