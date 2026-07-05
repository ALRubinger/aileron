package publish

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// dockerImageSource is the production ImageSource. It works entirely through
// the docker CLI and the operator's existing registry auth:
//
//   - ConfigDigest reads the local composed image's config digest (its image
//     Id), so publish can verify it against the signed lock BEFORE pushing.
//   - Push tags the local image into the destination repository and pushes it,
//     preserving the config blob digest so the pushed manifest's config digest
//     still equals the signed-lock digest.
//
// It deliberately does not use `docker save`: a docker export re-encodes the
// image (and older/newer docker CLIs differ on the OCI-layout flag), whereas a
// plain `docker push` is universally available and preserves the config digest
// the composed binding relies on.
type dockerImageSource struct{}

func (dockerImageSource) ConfigDigest(ctx context.Context, localTag string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", localTag).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w: %s", localTag, err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if !strings.HasPrefix(id, "sha256:") {
		return "", fmt.Errorf("docker image inspect %s: unexpected image id %q", localTag, id)
	}
	return id, nil
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
