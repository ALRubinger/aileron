package store

import (
	"errors"
	"os/exec"
	"testing"
)

var errClone = errors.New("clone failed")

// gitAvailable reports whether the git binary is resolvable on PATH.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// mustGit runs a git command in dir and fails the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, string(out))
	}
}
