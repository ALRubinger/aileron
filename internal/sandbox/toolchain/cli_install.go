package toolchain

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// devcontainerCLIPackage is the npm package name for the devcontainer CLI. The
// version pin is supplied per-install (from container.DevcontainerCLIVersion) so
// this package never carries its own copy of the pin.
const devcontainerCLIPackage = "@devcontainers/cli"

// cliEntrypointRelPath is the path, relative to an npm install prefix, of the
// devcontainer CLI's JS entrypoint. npm installs a package's files under
// <prefix>/lib/node_modules/<package>/, and @devcontainers/cli's package.json
// "bin" points at devcontainer.js under its bin/ directory.
var cliEntrypointRelPath = filepath.Join("lib", "node_modules", "@devcontainers", "cli", "devcontainer.js")

// cliCachePrefix is the version-keyed npm install prefix the CLI installer
// writes into: <cacheRoot>/devcontainer-cli/<cliVersion>. It is the single
// source of truth for that layout, reused by the offline resolver (via
// cliEntrypointForCache) so the offline probe and the installer can never drift.
func cliCachePrefix(cacheRoot, cliVersion string) string {
	return filepath.Join(cacheRoot, "devcontainer-cli", cliVersion)
}

// npmCLIInstaller is the production cliInstaller: it runs the managed node's
// bundled npm to install the pinned @devcontainers/cli into a version-keyed
// cache prefix, then resolves the CLI's JS entrypoint. The install is keyed by
// CLI version, so a warm cache (the entrypoint already present for that version)
// short-circuits without running npm again.
type npmCLIInstaller struct{}

// Install implements cliInstaller. The cache prefix is
// <cacheRoot>/devcontainer-cli/<cliVersion>; the entrypoint is the package's
// devcontainer.js under that prefix. A present entrypoint is returned from cache;
// otherwise npm installs the pinned package into the prefix and the entrypoint is
// returned with fromCache=false.
func (npmCLIInstaller) Install(ctx context.Context, nodeBinary, cliVersion, cacheRoot string) (string, bool, error) {
	prefix := cliCachePrefix(cacheRoot, cliVersion)
	entrypoint := filepath.Join(prefix, cliEntrypointRelPath)
	if fileExists(entrypoint) {
		return entrypoint, true, nil
	}
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		return "", false, fmt.Errorf("create CLI cache prefix %s: %w", prefix, err)
	}
	npmCLI, err := npmEntrypoint(nodeBinary)
	if err != nil {
		return "", false, err
	}
	// Run the managed node against its bundled npm to install the pinned package
	// into the version-keyed prefix. `--prefix` makes npm treat the prefix as a
	// global install root, landing files under <prefix>/lib/node_modules.
	spec := devcontainerCLIPackage + "@" + cliVersion
	args := []string{npmCLI, "install", "--global", "--prefix", prefix, "--no-audit", "--no-fund", spec}
	cmd := exec.CommandContext(ctx, nodeBinary, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("npm install %s: %w", spec, err)
	}
	if !fileExists(entrypoint) {
		return "", false, fmt.Errorf("npm install %s did not produce %s", spec, entrypoint)
	}
	return entrypoint, false, nil
}

// npmEntrypoint resolves the path to npm's CLI JS shipped alongside the managed
// node binary. A Node distribution bundles npm under
// <node-root>/lib/node_modules/npm/bin/npm-cli.js (Unix) or
// <node-root>/node_modules/npm/bin/npm-cli.js (Windows), where <node-root> is the
// directory above the node binary's own directory.
func npmEntrypoint(nodeBinary string) (string, error) {
	binDir := filepath.Dir(nodeBinary)
	// Unix: node is at <root>/bin/node, npm under <root>/lib/node_modules/npm.
	// Windows: node.exe is at <root>/node.exe, npm under <root>/node_modules/npm.
	candidates := []string{
		filepath.Join(filepath.Dir(binDir), "lib", "node_modules", "npm", "bin", "npm-cli.js"),
		filepath.Join(binDir, "node_modules", "npm", "bin", "npm-cli.js"),
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("bundled npm not found alongside managed node %s (looked in %v)", nodeBinary, candidates)
}
