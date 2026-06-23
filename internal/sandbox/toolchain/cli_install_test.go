package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func goosIsWindows() bool { return runtime.GOOS == "windows" }

func TestNpmInstallerCacheHitSkipsNpm(t *testing.T) {
	cacheRoot := t.TempDir()
	cliVersion := "9.9.9"
	// Pre-create the entrypoint so the warm-cache probe short-circuits before any
	// npm subprocess runs (a missing node binary would otherwise fail the exec).
	entrypoint := filepath.Join(cacheRoot, "devcontainer-cli", cliVersion, cliEntrypointRelPath)
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(entrypoint, []byte("// devcontainer cli"), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	got, fromCache, err := npmCLIInstaller{}.Install(context.Background(), "/nonexistent/node", cliVersion, cacheRoot)
	if err != nil {
		t.Fatalf("Install (warm cache): %v", err)
	}
	if !fromCache {
		t.Fatal("fromCache = false on a pre-populated cache; want true")
	}
	if got != entrypoint {
		t.Fatalf("entrypoint = %q, want %q", got, entrypoint)
	}
}

func TestNpmEntrypointResolvesUnixLayout(t *testing.T) {
	root := t.TempDir()
	nodeBinary := filepath.Join(root, "bin", "node")
	npmCLI := filepath.Join(root, "lib", "node_modules", "npm", "bin", "npm-cli.js")
	if err := os.MkdirAll(filepath.Dir(npmCLI), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(npmCLI, []byte("//npm"), 0o644); err != nil {
		t.Fatalf("write npm: %v", err)
	}
	got, err := npmEntrypoint(nodeBinary)
	if err != nil {
		t.Fatalf("npmEntrypoint: %v", err)
	}
	if got != npmCLI {
		t.Fatalf("npm cli = %q, want %q", got, npmCLI)
	}
}

func TestNpmEntrypointResolvesWindowsLayout(t *testing.T) {
	root := t.TempDir()
	nodeBinary := filepath.Join(root, "node.exe")
	npmCLI := filepath.Join(root, "node_modules", "npm", "bin", "npm-cli.js")
	if err := os.MkdirAll(filepath.Dir(npmCLI), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(npmCLI, []byte("//npm"), 0o644); err != nil {
		t.Fatalf("write npm: %v", err)
	}
	got, err := npmEntrypoint(nodeBinary)
	if err != nil {
		t.Fatalf("npmEntrypoint: %v", err)
	}
	if got != npmCLI {
		t.Fatalf("npm cli = %q, want %q", got, npmCLI)
	}
}

func TestNpmEntrypointMissingErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := npmEntrypoint(filepath.Join(root, "bin", "node")); err == nil {
		t.Fatal("npmEntrypoint: want error when bundled npm is absent, got nil")
	}
}

func TestNpmInstallerSurfacesNpmFailure(t *testing.T) {
	if goosIsWindows() {
		t.Skip("shell-script fake node is POSIX-only")
	}
	root := t.TempDir()
	// A fake node that always exits non-zero, plus a stub bundled npm so
	// npmEntrypoint resolves. Install must run the fake node, see it fail, and
	// surface an npm-install error rather than claiming success.
	binDir := filepath.Join(root, "node", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	nodeBinary := filepath.Join(binDir, "node")
	if err := os.WriteFile(nodeBinary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake node: %v", err)
	}
	npmCLI := filepath.Join(root, "node", "lib", "node_modules", "npm", "bin", "npm-cli.js")
	if err := os.MkdirAll(filepath.Dir(npmCLI), 0o755); err != nil {
		t.Fatalf("mkdir npm: %v", err)
	}
	if err := os.WriteFile(npmCLI, []byte("//npm"), 0o644); err != nil {
		t.Fatalf("write npm: %v", err)
	}
	cacheRoot := filepath.Join(root, "cache")
	_, _, err := npmCLIInstaller{}.Install(context.Background(), nodeBinary, "1.2.3", cacheRoot)
	if err == nil {
		t.Fatal("Install: want error when npm exits non-zero, got nil")
	}
}

func TestFileExists(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	if fileExists(f) {
		t.Fatal("fileExists true for a missing path")
	}
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !fileExists(f) {
		t.Fatal("fileExists false for an existing file")
	}
}
