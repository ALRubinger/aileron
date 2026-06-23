// Package toolchain provisions the Aileron-managed build toolchain for a
// Features (Tier 1) devcontainer build: a verified Node runtime plus the pinned
// @devcontainers/cli. It composes the nodedist fetcher (verified, cached Node
// distributions), the container package's tools.lock checksum guard, and a CLI
// installer into a single Provision call the container Builder invokes when a
// build selects the managed toolchain (#1525).
//
// It is deliberately separate from the container package so that package stays
// Docker-free and dependency-light, and so the nodedist + toolslock + CLI
// acquisition composition is independently testable. Every external dependency
// (HTTP fetch, CLI install) is behind a seam a unit test can fake, so the
// package's tests run with no network and no Docker.
package toolchain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
)

// cliInstaller acquires the pinned @devcontainers/cli into a cache prefix and
// returns the absolute path to its JS entrypoint. It is the one genuinely
// greenfield acquisition step, seamed so unit tests inject a fake and assert no
// network or Docker. cacheRoot is the toolchain cache root; the installer is
// expected to key its install by cliVersion so a warm cache short-circuits.
type cliInstaller interface {
	// Install ensures @devcontainers/cli@cliVersion is present in a cache prefix
	// under cacheRoot, using nodeBinary to run npm when an install is needed, and
	// returns the absolute path to the CLI's JS entrypoint plus whether it was
	// served from cache.
	Install(ctx context.Context, nodeBinary, cliVersion, cacheRoot string) (entrypoint string, fromCache bool, err error)
}

// Options configures a Provision call. Every field has a production default, so
// the zero value (plus a CacheRoot) provisions the pinned managed toolchain for
// the running host; tests override the seams (HTTP, Keyring, CLIInstaller) to
// run hermetically.
type Options struct {
	// CacheRoot is the content-addressed cache root. Defaults to
	// filepath.Join(cstore.DefaultRoot(), "toolchain"). Node distributions land
	// under <CacheRoot>/node; the CLI under <CacheRoot>/devcontainer-cli.
	CacheRoot string

	// HTTP downloads the Node archive + checksums. Defaults to a real
	// cstore.HTTPFetcher.
	HTTP cstore.Fetcher

	// Keyring is the trusted Node release keyring. Defaults to
	// nodedist.DefaultKeyring().
	Keyring nodedistKeyring

	// NodeVersion overrides the Node version to fetch. Defaults to the pinned
	// version from the committed tools.lock.json (container.PinnedNodeVersion).
	NodeVersion string

	// GOOS/GOARCH select the target platform. Default to the running host's.
	GOOS   string
	GOARCH string

	// CLIVersion overrides the @devcontainers/cli version. Defaults to
	// container.DevcontainerCLIVersion.
	CLIVersion string

	// CLIInstaller acquires the pinned CLI. Defaults to the npm-based installer.
	CLIInstaller cliInstaller
}

// nodedistKeyring is the keyring type Options.Keyring carries. It is an alias of
// the nodedist fetcher's keyring field type so callers do not import openpgp
// directly to set it.
type nodedistKeyring = func() (keyringList, error)

// Provision fetches and verifies the managed Node distribution, acquires the
// pinned @devcontainers/cli, and returns the resolved paths the container build
// invokes. It is idempotent: a warm cache (Node already cached, CLI entrypoint
// present) short-circuits without re-downloading. The tools.lock checksum is
// re-verified at the boundary on every call, including cache hits, so a tampered
// cache entry cannot satisfy a build.
//
// Provision returns container.ManagedToolchain so the Builder's
// toolchainProvisioner seam consumes it directly.
func Provision(ctx context.Context, opts Options) (container.ManagedToolchain, error) {
	resolved, err := opts.resolve()
	if err != nil {
		return container.ManagedToolchain{}, err
	}

	keyring, err := resolved.keyring()
	if err != nil {
		return container.ManagedToolchain{}, fmt.Errorf("load Node release keyring: %w", err)
	}

	fetcher := &nodedist.Fetcher{
		HTTP:    resolved.HTTP,
		Keyring: keyring,
		Root:    filepath.Join(resolved.CacheRoot, "node"),
	}
	dist, err := fetcher.Fetch(ctx, nodedist.Request{
		Version: resolved.NodeVersion,
		Target:  nodedist.Target{GOOS: resolved.GOOS, GOARCH: resolved.GOARCH},
	})
	if err != nil {
		return container.ManagedToolchain{}, fmt.Errorf("fetch managed Node %s: %w", resolved.NodeVersion, err)
	}

	// Re-verify against the committed tools.lock pin at the boundary, on every
	// call (cache hit or fresh download), so a tampered cache or a drifted lock
	// is caught before the toolchain is ever used.
	if err := container.VerifyNodeChecksum(dist.Version, resolved.GOOS, resolved.GOARCH, dist.SHA256); err != nil {
		return container.ManagedToolchain{}, err
	}

	entrypoint, _, err := resolved.CLIInstaller.Install(ctx, dist.NodeBinary, resolved.CLIVersion, resolved.CacheRoot)
	if err != nil {
		return container.ManagedToolchain{}, fmt.Errorf("install @devcontainers/cli@%s: %w", resolved.CLIVersion, err)
	}
	if entrypoint == "" {
		return container.ManagedToolchain{}, fmt.Errorf("CLI installer returned an empty entrypoint")
	}

	return container.ManagedToolchain{
		NodeBinary:    dist.NodeBinary,
		CLIEntrypoint: entrypoint,
	}, nil
}

// Provisioner adapts the package-level Provision function (with a fixed set of
// Options) to the container.Builder's toolchainProvisioner seam, which expects a
// value with a Provision(ctx) method. Callers construct it with the Options they
// want (typically the zero value plus a CacheRoot, taking every production
// default) and assign it to Builder.Provisioner.
type Provisioner struct {
	Options Options
}

// Provision implements the container.Builder toolchainProvisioner seam by
// delegating to the package-level Provision with the configured Options.
func (p Provisioner) Provision(ctx context.Context) (container.ManagedToolchain, error) {
	return Provision(ctx, p.Options)
}

// resolvedOptions is Options with every default applied.
type resolvedOptions struct {
	CacheRoot    string
	HTTP         cstore.Fetcher
	keyring      func() (keyringList, error)
	NodeVersion  string
	GOOS         string
	GOARCH       string
	CLIVersion   string
	CLIInstaller cliInstaller
}

func (o Options) resolve() (resolvedOptions, error) {
	r := resolvedOptions{
		CacheRoot:    o.CacheRoot,
		HTTP:         o.HTTP,
		keyring:      o.Keyring,
		NodeVersion:  o.NodeVersion,
		GOOS:         o.GOOS,
		GOARCH:       o.GOARCH,
		CLIVersion:   o.CLIVersion,
		CLIInstaller: o.CLIInstaller,
	}
	if r.CacheRoot == "" {
		r.CacheRoot = filepath.Join(cstore.DefaultRoot(), "toolchain")
	}
	if r.HTTP == nil {
		r.HTTP = &cstore.HTTPFetcher{}
	}
	if r.keyring == nil {
		r.keyring = defaultKeyring
	}
	if r.NodeVersion == "" {
		r.NodeVersion = container.PinnedNodeVersion()
	}
	if r.GOOS == "" {
		r.GOOS = goruntime.GOOS
	}
	if r.GOARCH == "" {
		r.GOARCH = goruntime.GOARCH
	}
	if r.CLIVersion == "" {
		r.CLIVersion = container.DevcontainerCLIVersion
	}
	if r.CLIInstaller == nil {
		r.CLIInstaller = npmCLIInstaller{}
	}
	return r, nil
}

// fileExists reports whether path exists and is a regular file or directory the
// caller can stat. Used by the npm installer's warm-cache probe.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
