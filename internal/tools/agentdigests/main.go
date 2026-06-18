// Command agentdigests regenerates the committed sandbox image digest lockfile
// (internal/sandbox/composition/agent-images.lock.json). It is run by
// `task generate:agent-digests` at release prep, after the per-agent images for
// the release have been published.
//
// For each published agent (composition.PublishedAgents) it resolves the
// immutable digest of "<repo>-<agent>:<tag>" from the registry via
// `docker buildx imagetools inspect` and writes the agent->digest map. The
// launcher then pulls "<repo>-<agent>@sha256:..." for release builds, so a
// release's sandbox is reproducible and cannot drift when the freshness watcher
// (#1088) republishes images (#1233).
//
// Usage:
//
//	go run ./tools/agentdigests -tag latest -out sandbox/composition/agent-images.lock.json
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ALRubinger/aileron/internal/sandbox/composition"
)

func main() {
	tag := flag.String("tag", "latest", "image tag to resolve each agent digest from (the tag the release published)")
	out := flag.String("out", "sandbox/composition/agent-images.lock.json", "lockfile path to write")
	flag.Parse()

	digests := make(map[string]string)
	for _, agent := range composition.PublishedAgents() {
		ref := fmt.Sprintf("%s-%s:%s", composition.DefaultPublishedImageRepository, agent, *tag)
		digest, err := resolveDigest(ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve %s: %v\n", ref, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "%s -> %s\n", ref, digest)
		digests[agent] = digest
	}

	data, err := composition.FormatAgentImageLock(digests)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render lockfile: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %d pin(s) to %s\n", len(digests), *out)
}

// resolveDigest returns the manifest digest ("sha256:...") of ref. It shells out
// to `docker buildx imagetools inspect`, which reports the top-level digest of a
// multi-arch manifest list — the same value a `@sha256:` pull resolves to.
func resolveDigest(ref string) (string, error) {
	cmd := exec.Command("docker", "buildx", "imagetools", "inspect", ref, "--format", "{{ .Manifest.Digest }}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	digest := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(digest, "sha256:") || len(digest) <= len("sha256:") {
		return "", fmt.Errorf("unexpected digest %q", digest)
	}
	return digest, nil
}
