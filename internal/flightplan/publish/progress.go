package publish

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/ALRubinger/aileron/internal/cli/progress"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

// dockerMediaTypeForeignLayer is the Docker foreign (non-distributable) layer
// media type. oras references it as docker.MediaTypeForeignLayer, but that lives
// in oras's internal/docker package and is not importable, so the constant is
// mirrored here. Keep it byte-identical to oras's value; the drift-guard test
// TestSumSubDAGSizeFiltersForeignLayers pins the whole foreign set so this stays
// in sync with oras's internal/descriptor.IsForeignLayer.
const dockerMediaTypeForeignLayer = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"

// isForeignLayer reports whether desc describes a foreign (non-distributable)
// layer that oras never copies. It mirrors oras's internal
// descriptor.IsForeignLayer set exactly: the three OCI non-distributable layer
// media types plus the Docker foreign layer type. oras strips these via
// removeForeignLayers (copy.go) before CopyGraph, so the determinate progress
// total must exclude their bytes or a push carrying a foreign layer can never
// settle the bar to 100%.
func isForeignLayer(desc ocispec.Descriptor) bool {
	switch desc.MediaType {
	case ocispec.MediaTypeImageLayerNonDistributable, //nolint:staticcheck // mirrors oras's IsForeignLayer set; the type is deprecated upstream but oras still filters it
		ocispec.MediaTypeImageLayerNonDistributableGzip, //nolint:staticcheck // see above
		ocispec.MediaTypeImageLayerNonDistributableZstd, //nolint:staticcheck // see above
		dockerMediaTypeForeignLayer:
		return true
	default:
		return false
	}
}

// sumSubDAGSize returns the total byte weight of the sub-DAG rooted at root:
// root.Size plus the Size of every transitive successor reachable via
// content.Successors, counting each distinct digest exactly once and skipping
// foreign/non-distributable layers oras never copies. An OCI graph is a DAG (a
// shared config or layer blob can be pointed at by several manifests), so
// deduping by digest is required to avoid double-counting a shared child. It is
// the precomputed determinate `total` a push settles against: a fully-fresh push
// settles it via PostCopy, a fully-already-present push via OnCopySkipped, and
// both land at exactly this value (100%). Foreign layers are excluded because
// oras strips them via removeForeignLayers before copying, so their bytes are
// never settled and counting them would leave the bar short of 100%.
func sumSubDAGSize(ctx context.Context, fetcher content.Fetcher, root ocispec.Descriptor) (int64, error) {
	seen := make(map[string]bool)
	var walk func(desc ocispec.Descriptor) (int64, error)
	walk = func(desc ocispec.Descriptor) (int64, error) {
		// A foreign layer is never copied by oras, so it contributes no settled
		// bytes and is excluded from the total. Foreign layers are leaf blobs
		// with no successors, so there is nothing further to descend into.
		if isForeignLayer(desc) {
			return 0, nil
		}
		key := desc.Digest.String()
		if seen[key] {
			return 0, nil
		}
		seen[key] = true
		total := desc.Size
		succ, err := content.Successors(ctx, fetcher, desc)
		if err != nil {
			return 0, err
		}
		for _, s := range succ {
			n, err := walk(s)
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	}
	return walk(root)
}

// settledCounter is the shared running total of bytes a push has settled,
// incremented from the oras copy callbacks (which oras fires concurrently, up
// to CopyGraphOptions.Concurrency) and pushed into the Indicator's determinate
// display. The Indicator.Update itself is concurrency-safe; the counter is
// guarded so concurrent adds do not lose an increment.
type settledCounter struct {
	ind   *progress.Indicator
	total int64
	n     int64
}

// add advances the settled counter by delta and refreshes the determinate
// display. It is safe for concurrent use.
func (c *settledCounter) add(delta int64) {
	settled := atomic.AddInt64(&c.n, delta)
	c.ind.Update(settled, c.total)
}

// pushProgress builds the CopyGraphOptions that drive a determinate push
// against ind, settling `total` bytes. It starts from oras.DefaultCopyGraphOptions
// (preserving the default Concurrency of 3) and sets only the settled-byte hooks:
//
//   - PostCopy(desc) adds desc.Size once, for each freshly-copied node.
//   - OnCopySkipped(desc) adds the WHOLE sub-DAG weight of desc once, because
//     oras fires OnCopySkipped a single time per already-present sub-DAG ROOT
//     and does not descend into its successors. Summing the skipped root's
//     sub-DAG here is what keeps a fully-already-present push settling at
//     exactly total (100%) rather than stalling at the root blob's size.
//
// A fetcher over the SOURCE store is needed to walk a skipped sub-DAG's weight;
// the sizes come from the source descriptors, which are byte-identical to what
// would have been copied. The returned options carry the shared counter's hooks;
// the caller holds ind and resolves it (Done/Fail) around the copy.
func pushProgress(fetcher content.Fetcher, ind *progress.Indicator, total int64) oras.CopyGraphOptions {
	opts := oras.DefaultCopyGraphOptions
	counter := &settledCounter{ind: ind, total: total}
	// seen guards against adding a skipped digest's weight more than once. oras
	// commits each descriptor exactly once before firing OnCopySkipped, so a
	// second firing for the same digest should not occur; this set makes the
	// once-only accounting explicit and robust regardless, so a repeated skip
	// (or a shared child skipped under two parents) never double-settles. The
	// mutex guards the set against the concurrent callbacks and reserves the key
	// BEFORE the sumSubDAGSize walk so a concurrent duplicate observes it taken.
	var seenMu sync.Mutex
	seen := make(map[string]bool)
	opts.PostCopy = func(ctx context.Context, desc ocispec.Descriptor) error {
		counter.add(desc.Size)
		return nil
	}
	opts.OnCopySkipped = func(ctx context.Context, desc ocispec.Descriptor) error {
		key := desc.Digest.String()
		seenMu.Lock()
		if seen[key] {
			seenMu.Unlock()
			return nil
		}
		seen[key] = true
		seenMu.Unlock()

		weight, err := sumSubDAGSize(ctx, fetcher, desc)
		if err != nil {
			// A weight walk failure must not fail the push (the copy itself
			// succeeded); fall back to the root's own size so progress still
			// advances rather than stalling, and never returns an error that
			// oras would treat as a copy failure.
			weight = desc.Size
		}
		counter.add(weight)
		return nil
	}
	return opts
}
