package publish

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"
)

// dockerImageSource is the production ImageSource. It works entirely through
// the docker CLI and the operator's existing registry auth:
//
//   - ConfigContentDigest reads the local composed image's serialization-
//     agnostic config content digest (see internal/flightplan/imgconfig) from
//     `docker image inspect`, so publish can verify it against the signed lock
//     BEFORE pushing.
//   - Push tags the local image into the destination repository and pushes it.
//
// It deliberately does not use `docker save`: a docker export re-encodes the
// image (and older/newer docker CLIs differ on the OCI-layout flag), whereas a
// plain `docker push` is universally available. The composed binding no longer
// depends on the config-blob digest being preserved byte-for-byte across push:
// it binds to the config CONTENT digest, which is stable across the containerd
// store's push-time re-serialization (issue #2014).
type dockerImageSource struct{}

func (dockerImageSource) ConfigContentDigest(ctx context.Context, localTag string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .}}", localTag).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w: %s", localTag, err, strings.TrimSpace(string(out)))
	}
	cc, err := imgconfig.FromDockerInspect(out)
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w", localTag, err)
	}
	return cc.ContentDigest()
}

func (dockerImageSource) Push(ctx context.Context, localTag, registry, imageTag string) error {
	ref := registry + ":" + imageTag
	if out, err := exec.CommandContext(ctx, "docker", "tag", localTag, ref).CombinedOutput(); err != nil {
		return fmt.Errorf("docker tag %s %s: %w: %s", localTag, ref, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "docker", "push", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("docker push %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}
