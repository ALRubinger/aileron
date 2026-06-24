package toolchain

import (
	"fmt"
	"path/filepath"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
)

// provisionOffline resolves the managed toolchain entirely from the warm
// content-addressed cache, with zero network access. It is the Offline=true
// branch of Provision: it never constructs the nodedist.Fetcher (which always
// downloads the signed checksums to learn the expected hash) nor runs the CLI
// installer. Instead it computes the Node cache key from the committed
// tools.lock pin, serves the cached Node tree and the cached CLI entrypoint
// directly, and re-asserts the cached hash against the pin as a cheap tamper
// guard. A cold cache (Node tree or CLI entrypoint absent) fails with an
// actionable error naming `aileron sandbox warm`.
func provisionOffline(resolved resolvedOptions) (container.ManagedToolchain, error) {
	nodeCacheRoot := filepath.Join(resolved.CacheRoot, "node")
	node, err := resolveCachedNode(nodeCacheRoot, resolved.NodeVersion, resolved.GOOS, resolved.GOARCH)
	if err != nil {
		return container.ManagedToolchain{}, err
	}

	// Re-verify the cached hash against the committed pin. The hash came from the
	// pin, so this is a consistency assert (cheap and defensive): it keeps the
	// offline path's trust anchor identical to the online boundary guard.
	if err := container.VerifyNodeChecksum(resolved.NodeVersion, resolved.GOOS, resolved.GOARCH, node.SHA256); err != nil {
		return container.ManagedToolchain{}, err
	}

	entrypoint := cliEntrypointForCache(resolved.CacheRoot, resolved.CLIVersion)
	if !fileExists(entrypoint) {
		return container.ManagedToolchain{}, offlineMissingCLIError(resolved.CLIVersion)
	}

	return container.ManagedToolchain{
		NodeBinary:    node.NodeBinary,
		CLIEntrypoint: entrypoint,
	}, nil
}

// cachedNode is a managed Node distribution resolved from the content-addressed
// cache without any network access. It carries the same fields the online
// nodedist.Fetcher returns that the offline Provision path needs: the verified
// sha256 (taken from the pin) and the absolute node binary path.
type cachedNode struct {
	// SHA256 is the pinned, content-addressed sha256 of the Node tree — the
	// cache key. It comes from the committed tools.lock pin, so re-verifying it
	// against the pin is a cheap consistency assert.
	SHA256 string
	// NodeBinary is the absolute path to the node executable inside the cached
	// entry.
	NodeBinary string
}

// resolveCachedNode resolves a managed Node distribution from the warm cache for
// version on goos/goarch, with zero network access. It computes the cache key
// from the committed tools.lock pin (the pinned sha256 is exactly the cstore
// key), probes the content-addressed store for that tree, and returns the cached
// node binary path. It returns an actionable error naming `aileron sandbox warm`
// when the tree is absent (a cold cache), and reuses the container package's
// pin-vs-platform error shape when the platform is unsupported or unpinned.
//
// nodeCacheRoot is the Node cache subtree (`<CacheRoot>/node`), matching the
// Root the online Fetcher uses.
func resolveCachedNode(nodeCacheRoot, version, goos, goarch string) (cachedNode, error) {
	hash, ok := container.PinnedNodeChecksum(version, goos, goarch)
	if !ok {
		// Reuse the container package's actionable pin/platform error shape so
		// the offline path reports unsupported/unpinned platforms identically to
		// the online boundary guard. With no pin, VerifyNodeChecksum reports the
		// unsupported/unpinned error before it ever compares the actual sha, so
		// the empty actual is never inspected. Guard against a future change that
		// returns nil here so the resolver can never report success with no pin.
		if err := container.VerifyNodeChecksum(version, goos, goarch, ""); err != nil {
			return cachedNode{}, err
		}
		return cachedNode{}, fmt.Errorf("managed Node %s (%s/%s) has no pinned checksum; the offline cache cannot be resolved", version, goos, goarch)
	}

	present, err := cstore.HasDirHash(nodeCacheRoot, hash)
	if err != nil {
		return cachedNode{}, fmt.Errorf("probe offline Node cache for %s: %w", hash, err)
	}
	if !present {
		return cachedNode{}, offlineMissingNodeError(version, goos, goarch)
	}

	entryDir, err := cstore.DirEntryDir(nodeCacheRoot, hash)
	if err != nil {
		return cachedNode{}, fmt.Errorf("resolve offline Node cache entry for %s: %w", hash, err)
	}
	return cachedNode{
		SHA256:     hash,
		NodeBinary: nodedist.NodeBinaryPath(entryDir, nodedist.Target{GOOS: goos, GOARCH: goarch}),
	}, nil
}

// offlineMissingNodeError is the actionable error for a cold Node cache in
// offline mode: it names the version, the platform, and the `aileron sandbox
// warm` remedy that populates the cache with network access.
func offlineMissingNodeError(version, goos, goarch string) error {
	return fmt.Errorf("managed Node %s (%s/%s) is not in the offline cache; run `aileron sandbox warm` with network access first", version, goos, goarch)
}

// offlineMissingCLIError is the actionable error for a missing @devcontainers/cli
// entrypoint in offline mode: the Node tree was cached but the CLI was never
// installed, so warming is required to populate it.
func offlineMissingCLIError(cliVersion string) error {
	return fmt.Errorf("managed @devcontainers/cli@%s is not in the offline cache; run `aileron sandbox warm` with network access first", cliVersion)
}

// cliEntrypointForCache returns the absolute path the npm installer would place
// the @devcontainers/cli entrypoint at for cliVersion under cacheRoot. The
// offline path probes this without running npm; it must match the layout
// npmCLIInstaller.Install writes (see cli_install.go).
func cliEntrypointForCache(cacheRoot, cliVersion string) string {
	return filepath.Join(cacheRoot, "devcontainer-cli", cliVersion, cliEntrypointRelPath)
}
