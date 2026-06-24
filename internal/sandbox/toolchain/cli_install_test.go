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
	entrypoint := filepath.Join(cacheRoot, "devcontainer-cli", cliVersion, cliEntrypointRelPaths[0])
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

// TestNpmInstallerFindsWindowsLayoutEntrypoint is the regression for the
// reported Windows failure: `npm install --global --prefix <prefix>` lands
// @devcontainers/cli at <prefix>/node_modules/... (no lib/ segment) on Windows,
// but the warm-cache probe only checked the Unix lib/node_modules/ layout, so it
// reported "did not produce ...lib\\node_modules..." for a package npm had just
// installed. With the Windows candidate probed, the staged Windows-layout
// entrypoint is found and Install short-circuits from cache without running npm.
func TestNpmInstallerFindsWindowsLayoutEntrypoint(t *testing.T) {
	cacheRoot := t.TempDir()
	cliVersion := "9.9.9"
	// Stage the entrypoint at the Windows layout (<prefix>/node_modules/..., no
	// lib/ segment), exactly where `npm install --prefix` lands it on Windows.
	windowsEntrypoint := filepath.Join(
		cacheRoot, "devcontainer-cli", cliVersion,
		"node_modules", "@devcontainers", "cli", "devcontainer.js",
	)
	if err := os.MkdirAll(filepath.Dir(windowsEntrypoint), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(windowsEntrypoint, []byte("// devcontainer cli"), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	// A nonexistent node would fail any npm subprocess; the probe must find the
	// staged file and short-circuit before exec.
	got, fromCache, err := npmCLIInstaller{}.Install(context.Background(), "/nonexistent/node", cliVersion, cacheRoot)
	if err != nil {
		t.Fatalf("Install (warm Windows-layout cache): %v", err)
	}
	if !fromCache {
		t.Fatal("fromCache = false for a staged Windows-layout entrypoint; want true")
	}
	if got != windowsEntrypoint {
		t.Fatalf("entrypoint = %q, want %q", got, windowsEntrypoint)
	}
}

// TestCLIEntrypointForPrefixPrefersUnixThenWindows pins the probe's contract:
// the Unix layout wins when present, the Windows layout is found when only it
// exists, and a missing entrypoint falls back to the canonical Unix path with
// found=false so error messages stay stable across platforms.
func TestCLIEntrypointForPrefixPrefersUnixThenWindows(t *testing.T) {
	unixRel := filepath.Join("lib", "node_modules", "@devcontainers", "cli", "devcontainer.js")
	windowsRel := filepath.Join("node_modules", "@devcontainers", "cli", "devcontainer.js")

	t.Run("missing returns canonical Unix path, not found", func(t *testing.T) {
		prefix := t.TempDir()
		got, found := cliEntrypointForPrefix(prefix)
		if found {
			t.Fatalf("found = true for an empty prefix; want false")
		}
		if want := filepath.Join(prefix, unixRel); got != want {
			t.Fatalf("path = %q, want canonical Unix %q", got, want)
		}
	})

	t.Run("windows-only layout is found", func(t *testing.T) {
		prefix := t.TempDir()
		win := filepath.Join(prefix, windowsRel)
		if err := os.MkdirAll(filepath.Dir(win), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(win, []byte("//cli"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, found := cliEntrypointForPrefix(prefix)
		if !found {
			t.Fatalf("found = false for a staged Windows layout; want true")
		}
		if got != win {
			t.Fatalf("path = %q, want %q", got, win)
		}
	})

	t.Run("unix layout wins when both present", func(t *testing.T) {
		prefix := t.TempDir()
		for _, rel := range []string{unixRel, windowsRel} {
			p := filepath.Join(prefix, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(p, []byte("//cli"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		got, found := cliEntrypointForPrefix(prefix)
		if !found {
			t.Fatalf("found = false when both layouts present; want true")
		}
		if want := filepath.Join(prefix, unixRel); got != want {
			t.Fatalf("path = %q, want Unix %q to win", got, want)
		}
	})
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
