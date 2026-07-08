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

// sumSubDAGSize returns the total byte weight of the sub-DAG rooted at root:
// root.Size plus the Size of every transitive successor reachable via
// content.Successors, counting each distinct digest exactly once. An OCI graph
// is a DAG (a shared config or layer blob can be pointed at by several
// manifests), so deduping by digest is required to avoid double-counting a
// shared child. It is the precomputed determinate `total` a push settles
// against: a fully-fresh push settles it via PostCopy, a fully-already-present
// push via OnCopySkipped, and both land at exactly this value (100%).
func sumSubDAGSize(ctx context.Context, fetcher content.Fetcher, root ocispec.Descriptor) (int64, error) {
	seen := make(map[string]bool)
	var walk func(desc ocispec.Descriptor) (int64, error)
	walk = func(desc ocispec.Descriptor) (int64, error) {
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
