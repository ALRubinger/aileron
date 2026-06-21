package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// exeSuffix is the executable extension exec.LookPath requires when
// finding a binary on PATH: ".exe" on Windows, empty elsewhere. PATH
// lookup ignores extensionless files on Windows, so staged fake
// binaries must carry this suffix to be discoverable.
var exeSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()

// setTestHome redirects the process home directory to a fresh temp dir
// for the duration of the test and returns that dir.
//
// os.UserHomeDir() reads $HOME on Unix but %USERPROFILE% on Windows
// (with %HOMEDRIVE%+%HOMEPATH% as a fallback). Setting HOME alone
// leaves the Windows runner's real home in play, so this helper sets
// all of them at once. Tests that need the directory should use the
// returned value rather than re-reading os.Getenv("HOME"), which is
// not the source of truth on Windows.
func setTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(dir)
		t.Setenv("HOMEDRIVE", vol)
		t.Setenv("HOMEPATH", dir[len(vol):])
	}
	return dir
}

// stageServerOnPath writes a no-op "aileron-server" binary into a fresh
// temp dir, prepends that dir to PATH, and returns the dir. The binary
// is named with exeSuffix so exec.LookPath finds it on every platform.
// Daemon-start tests mock the spawn seam, so the binary is never
// executed; only exec.LookPath must locate it.
func stageServerOnPath(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	server := filepath.Join(binDir, "aileron-server"+exeSuffix)
	if err := os.WriteFile(server, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir
}
