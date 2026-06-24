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

// runtimeDigestResolver resolves a rung-1 image reference to its digest by
// pulling (best-effort) then inspecting RepoDigests through the shared
// container Runner seam. It pins by digest, never by tag.
type runtimeDigestResolver struct{}

func (runtimeDigestResolver) ResolveDigest(ctx context.Context, ref string) (string, error) {
	runner := container.DefaultRunner()
	runtimeName, err := container.ResolveRuntime(container.DefaultRuntime)
	if err != nil {
		return "", fmt.Errorf("resolve container runtime: %w", err)
	}

	// Best-effort pull so an image present only in a remote registry can be
	// inspected. A pull failure is not fatal here: the image may already be
	// local, in which case the inspect below still yields a digest.
	_ = runner.Run(ctx, runtimeName, []string{"pull", ref}, io.Discard, io.Discard)

	var out bytes.Buffer
	if err := runner.Run(ctx, runtimeName, []string{"image", "inspect", "--format", "{{json .RepoDigests}}", ref}, &out, io.Discard); err != nil {
		return "", fmt.Errorf("inspect image %q: %w", ref, err)
	}
	return digestFromRepoDigests(out.Bytes(), ref)
}

// resolvedRuntime resolves the default container runtime name, returning an
// error when no supported runtime is on PATH.
func resolvedRuntime() (string, error) {
	return container.ResolveRuntime(container.DefaultRuntime)
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
	// Compose the Features onto the Aileron base image via the standard
	// composition Plan, build through the Builder, then resolve the built
	// image's digest with the same digest resolver used for rung-1.
	plan := composition.Plan{
		Tier:     composition.TierDevcontainer,
		Features: map[string]json.RawMessage{},
	}
	for _, f := range features {
		plan.Features[f] = json.RawMessage("{}")
	}

	b := container.Builder{
		Runtime: container.DefaultRuntime,
		Runner:  container.DefaultRunner(),
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	}
	result, err := b.Build(ctx, container.BuildOptions{
		Plan:   plan,
		Policy: container.BuildPolicyAlways,
	})
	if err != nil {
		return "", fmt.Errorf("compose rung-2 features: %w", err)
	}
	// The built image is a local tag; resolve it to a digest. A locally-built
	// image typically has no RepoDigests, so fall back to its image Id.
	return localImageDigest(ctx, result.Image)
}

// localImageDigest resolves a locally-built image tag to a content digest.
// It prefers the registry RepoDigests pin, then falls back to the local
// image Id (also a `sha256:` content address) for an image that was never
// pushed.
func localImageDigest(ctx context.Context, image string) (string, error) {
	runner := container.DefaultRunner()
	runtimeName, err := resolvedRuntime()
	if err != nil {
		return "", fmt.Errorf("resolve container runtime: %w", err)
	}

	var rd bytes.Buffer
	if err := runner.Run(ctx, runtimeName, []string{"image", "inspect", "--format", "{{json .RepoDigests}}", image}, &rd, io.Discard); err == nil {
		if digest, derr := digestFromRepoDigests(rd.Bytes(), image); derr == nil {
			return digest, nil
		}
	}

	var id bytes.Buffer
	if err := runner.Run(ctx, runtimeName, []string{"image", "inspect", "--format", "{{.Id}}", image}, &id, io.Discard); err != nil {
		return "", fmt.Errorf("resolve digest for built image %q: %w", image, err)
	}
	digest := strings.TrimSpace(id.String())
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("built image %q reported a non-sha256 Id %q", image, digest)
	}
	return digest, nil
}
