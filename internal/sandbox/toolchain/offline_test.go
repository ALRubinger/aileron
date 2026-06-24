package toolchain

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/container"
)

// failingFetcher is a cstore.Fetcher that fails the test if its Fetch is ever
// invoked. The offline path must never touch the network, so any call is a bug.
type failingFetcher struct{ t *testing.T }

func (f failingFetcher) Fetch(_ context.Context, url string) (io.ReadCloser, error) {
	f.t.Fatalf("offline Provision attempted a network fetch for %s; offline must use the cache only", url)
	return nil, errors.New("unreachable")
}

// failingCLIInstaller fails the test if Install is invoked. Offline must serve
// the CLI entrypoint from the cache without running the installer.
type failingCLIInstaller struct{ t *testing.T }

func (f failingCLIInstaller) Install(_ context.Context, _, _, _ string) (string, bool, error) {
	f.t.Fatal("offline Provision ran the CLI installer; offline must use the cache only")
	return "", false, errors.New("unreachable")
}

// stageCLIEntrypoint pre-populates the warm CLI cache for cliVersion under
// cacheRoot, mirroring what npmCLIInstaller.Install leaves on disk, so the
// offline path's entrypoint probe finds it.
func stageCLIEntrypoint(t *testing.T, cacheRoot, cliVersion string) string {
	t.Helper()
	entrypoint := cliEntrypointForCache(cacheRoot, cliVersion)
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatalf("mkdir CLI prefix: %v", err)
	}
	if err := os.WriteFile(entrypoint, []byte("// devcontainer.js\n"), 0o644); err != nil {
		t.Fatalf("write CLI entrypoint: %v", err)
	}
	return entrypoint
}

func TestProvisionOfflineFromWarmCacheNoNetwork(t *testing.T) {
	cacheRoot := t.TempDir()
	hash := pinnedChecksumForLinuxX64(t)
	stagedNode(t, cacheRoot, hash)
	cliEntrypoint := stageCLIEntrypoint(t, cacheRoot, container.DevcontainerCLIVersion)

	managed, err := Provision(context.Background(), Options{
		CacheRoot:    cacheRoot,
		HTTP:         failingFetcher{t},
		CLIInstaller: failingCLIInstaller{t},
		GOOS:         "linux",
		GOARCH:       "amd64",
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("offline Provision: %v", err)
	}
	wantNode := filepath.Join(cacheRoot, "node", "sha256", hash, "bin", "node")
	if managed.NodeBinary != wantNode {
		t.Fatalf("NodeBinary = %q, want %q", managed.NodeBinary, wantNode)
	}
	if managed.CLIEntrypoint != cliEntrypoint {
		t.Fatalf("CLIEntrypoint = %q, want %q", managed.CLIEntrypoint, cliEntrypoint)
	}
}

// Regression guard for the named acceptance: offline against a cold cache fails
// with an actionable message that names `warm` and the missing component, and
// never attempts a network fetch (the failingFetcher would Fatal if it did).
func TestProvisionOfflineColdCacheFailsActionably(t *testing.T) {
	cacheRoot := t.TempDir() // empty: no Node tree, no CLI

	_, err := Provision(context.Background(), Options{
		CacheRoot:    cacheRoot,
		HTTP:         failingFetcher{t},
		CLIInstaller: failingCLIInstaller{t},
		GOOS:         "linux",
		GOARCH:       "amd64",
		Offline:      true,
	})
	if err == nil {
		t.Fatal("offline Provision on a cold cache: want an actionable error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "aileron sandbox warm") {
		t.Fatalf("error %q does not name `aileron sandbox warm`", msg)
	}
	if !strings.Contains(msg, "Node") {
		t.Fatalf("error %q does not name the missing Node component", msg)
	}
}

// Offline with the Node tree cached but the CLI prefix absent must report the
// CLI as the missing component, also pointing at `warm`.
func TestProvisionOfflineCLIMissingFailsActionably(t *testing.T) {
	cacheRoot := t.TempDir()
	hash := pinnedChecksumForLinuxX64(t)
	stagedNode(t, cacheRoot, hash) // Node present, CLI deliberately absent

	_, err := Provision(context.Background(), Options{
		CacheRoot:    cacheRoot,
		HTTP:         failingFetcher{t},
		CLIInstaller: failingCLIInstaller{t},
		GOOS:         "linux",
		GOARCH:       "amd64",
		Offline:      true,
	})
	if err == nil {
		t.Fatal("offline Provision with no CLI: want an actionable error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "aileron sandbox warm") {
		t.Fatalf("error %q does not name `aileron sandbox warm`", msg)
	}
	if !strings.Contains(msg, "@devcontainers/cli") {
		t.Fatalf("error %q does not name the missing CLI component", msg)
	}
}

// Offline on an unsupported/unpinned platform reuses the container package's
// pin/platform error shape rather than a cache-miss message.
func TestProvisionOfflineUnsupportedPlatform(t *testing.T) {
	cacheRoot := t.TempDir()
	_, err := Provision(context.Background(), Options{
		CacheRoot:    cacheRoot,
		HTTP:         failingFetcher{t},
		CLIInstaller: failingCLIInstaller{t},
		GOOS:         "plan9",
		GOARCH:       "mips",
		Offline:      true,
	})
	if err == nil {
		t.Fatal("offline Provision on an unsupported platform: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("error %q is not the pin/platform error shape", err.Error())
	}
}
