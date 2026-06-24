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

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"
	"github.com/ALRubinger/aileron/internal/sandbox/container"
)

// newDigestResolver and newFeatureComposer are package-level seams so CLI
// tests exercise the freeze orchestration without a container runtime. The
// production implementations wire container.DefaultRunner() (rung-1 image
// inspect) and container.Builder (rung-2 Feature composition).
var newDigestResolver = func() freeze.DigestResolver { return runtimeDigestResolver{} }
var newFeatureComposer = func() freeze.FeatureComposer { return builderFeatureComposer{} }

func runSkillFreeze(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill freeze", flag.ContinueOnError)
	flags.SetOutput(stderr)
	signingKey := flags.String("signing-key", "", "Path to the PEM ed25519 signing key (falls back to $"+freeze.SigningKeyEnv+")")
	version := flags.String("version", "", "Semver label recorded in the lock (for example 1.0.0)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	target := flags.Arg(0)

	raw, err := readSkillForFreeze(target)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	res, err := freeze.Run(context.Background(), raw, freeze.Options{
		Version:        *version,
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
	// Surface any execution-environment rung that was declared but
	// intentionally not built (today: the reserved rung-3 slot). The operator
	// is told explicitly rather than left to assume the image set is complete.
	for _, rung := range res.DeferredRungs {
		fmt.Fprintf(stdout, "  Rung 3 declared: build-deferred (reserved manifest slot %q, ADR-0027); no image built\n", rung)
	}
	return 0
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
	return imageInspector{runner: container.DefaultRunner(), runtime: runtimeName}, nil
}

// pull best-effort pulls ref. A failure is intentionally non-fatal: the
// image may already be local, in which case the inspect still yields a
// digest.
func (in imageInspector) pull(ctx context.Context, ref string) {
	_ = in.runner.Run(ctx, in.runtime, []string{"pull", ref}, io.Discard, io.Discard)
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

// runtimeDigestResolver resolves a rung-1 image reference to its digest by
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

// builderFeatureComposer composes a rung-2 capability-unit Feature set
// through the container Builder (the #1454 build pipeline) and returns the
// built image's digest. It routes rung-2 through the same Builder /
// composition path the sandbox uses, not a bespoke build.
type builderFeatureComposer struct{}

func (builderFeatureComposer) ComposeDigest(ctx context.Context, features []string) (string, error) {
	in, err := newImageInspector()
	if err != nil {
		return "", err
	}
	// Compose the Features onto the Aileron base image via the standard
	// composition Plan, build through the Builder, then resolve the built
	// image's digest with the same inspector used for rung-1.
	b := container.Builder{
		Runtime: in.runtime,
		Runner:  in.runner,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	}
	result, err := b.Build(ctx, container.BuildOptions{
		Plan:   rung2Plan(features),
		Policy: container.BuildPolicyAlways,
	})
	if err != nil {
		return "", fmt.Errorf("compose rung-2 features: %w", err)
	}
	// The built image is a local tag; resolve it to a digest. A locally-built
	// image typically has no RepoDigests, so fall back to its image Id.
	return in.localImageDigest(ctx, result.Image)
}

// rung2Plan builds the composition Plan that composes the given
// capability-unit Features onto the Aileron base image. Routing rung-2
// through composition.Plan + the Builder keeps freeze on the same build
// path the sandbox uses rather than a bespoke build.
func rung2Plan(features []string) composition.Plan {
	plan := composition.Plan{
		Tier:     composition.TierDevcontainer,
		Features: make(map[string]json.RawMessage, len(features)),
	}
	for _, f := range features {
		plan.Features[f] = json.RawMessage("{}")
	}
	return plan
}

// localImageDigest resolves a locally-built image tag to a content digest.
// It prefers the registry RepoDigests pin, then falls back to the local
// image Id (also a `sha256:` content address) for an image that was never
// pushed.
func (in imageInspector) localImageDigest(ctx context.Context, image string) (string, error) {
	if rd, err := in.inspectFormat(ctx, image, "{{json .RepoDigests}}"); err == nil {
		if digest, derr := digestFromRepoDigests(rd, image); derr == nil {
			return digest, nil
		}
	}

	id, err := in.inspectFormat(ctx, image, "{{.Id}}")
	if err != nil {
		return "", fmt.Errorf("resolve digest for built image %q: %w", image, err)
	}
	digest := strings.TrimSpace(string(id))
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("built image %q reported a non-sha256 Id %q", image, digest)
	}
	return digest, nil
}
