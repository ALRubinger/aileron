package publish

import (
	"context"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
)

// composedLayoutOpener is the production ComposedLayout seam. The composed image
// is built at freeze for every supported architecture into a deterministic,
// tag-keyed OCI image-layout directory (composition.OCILayoutDir over the pin's
// LocalTag), never loaded through the docker daemon (the classic docker store
// cannot even hold a manifest list). This opens that layout as a read-only oras
// store and returns it with its root manifest-list descriptor, so publish both
// verifies every arch's config content digest against the signed lock and copies
// the full manifest-list graph into the destination registry straight from the
// layout, with no daemon round-trip and no config-blob re-serialization.
func composedLayoutOpener(ctx context.Context, pin freeze.ImagePin) (oras.ReadOnlyTarget, ocispec.Descriptor, error) {
	dir, err := composition.OCILayoutDir(pin.LocalTag)
	if err != nil {
		return nil, ocispec.Descriptor{}, err
	}
	return ociremote.OpenOCILayout(ctx, dir)
}
