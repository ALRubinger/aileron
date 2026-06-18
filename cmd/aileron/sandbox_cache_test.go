package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func swapCacheFns(t *testing.T, list func(context.Context, string) ([]string, error), remove func(context.Context, string, []string) error) {
	t.Helper()
	origList, origRemove := sandboxCacheListFn, sandboxCacheRemoveFn
	t.Cleanup(func() {
		sandboxCacheListFn = origList
		sandboxCacheRemoveFn = origRemove
	})
	sandboxCacheListFn = list
	sandboxCacheRemoveFn = remove
}

func TestRunSandboxCacheClearRemovesVolumes(t *testing.T) {
	var removed []string
	swapCacheFns(t,
		func(context.Context, string) ([]string, error) {
			return []string{"aileron-cache-npm-abc", "aileron-cache-pip-def"}, nil
		},
		func(_ context.Context, _ string, names []string) error {
			removed = names
			return nil
		},
	)
	var out, errb bytes.Buffer
	if code := runSandboxCacheClear([]string{"--runtime=docker"}, &out, &errb); code != 0 {
		t.Fatalf("runSandboxCacheClear = %d, stderr: %s", code, errb.String())
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want 2", removed)
	}
	got := out.String()
	if !strings.Contains(got, "removed aileron-cache-npm-abc") || !strings.Contains(got, "removed 2 cache volume(s)") {
		t.Fatalf("unexpected stdout: %q", got)
	}
}

func TestRunSandboxCacheClearNoVolumes(t *testing.T) {
	removeCalled := false
	swapCacheFns(t,
		func(context.Context, string) ([]string, error) { return nil, nil },
		func(context.Context, string, []string) error { removeCalled = true; return nil },
	)
	var out, errb bytes.Buffer
	if code := runSandboxCacheClear([]string{"--runtime=docker"}, &out, &errb); code != 0 {
		t.Fatalf("runSandboxCacheClear = %d, stderr: %s", code, errb.String())
	}
	if removeCalled {
		t.Fatal("remove must not run when there are no volumes")
	}
	if !strings.Contains(out.String(), "no Aileron-managed sandbox cache volumes") {
		t.Fatalf("unexpected stdout: %q", out.String())
	}
}

func TestRunSandboxCacheClearListError(t *testing.T) {
	swapCacheFns(t,
		func(context.Context, string) ([]string, error) { return nil, errors.New("boom") },
		func(context.Context, string, []string) error { return nil },
	)
	var out, errb bytes.Buffer
	if code := runSandboxCacheClear([]string{"--runtime=docker"}, &out, &errb); code != 1 {
		t.Fatalf("runSandboxCacheClear = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "boom") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestRunSandboxCacheClearRemoveError(t *testing.T) {
	swapCacheFns(t,
		func(context.Context, string) ([]string, error) { return []string{"aileron-cache-npm-abc"}, nil },
		func(context.Context, string, []string) error { return errors.New("rm failed") },
	)
	var out, errb bytes.Buffer
	if code := runSandboxCacheClear([]string{"--runtime=docker"}, &out, &errb); code != 1 {
		t.Fatalf("runSandboxCacheClear = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "rm failed") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestRunSandboxCacheRejectsExtraArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runSandboxCacheClear([]string{"extra"}, &out, &errb); code != 1 {
		t.Fatalf("runSandboxCacheClear = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestRunSandboxCacheUnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runSandboxCache([]string{"bogus"}, &out, &errb); code != 1 {
		t.Fatalf("runSandboxCache = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "unknown sandbox cache command") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestRunSandboxCacheNoSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runSandboxCache(nil, &out, &errb); code != 1 {
		t.Fatalf("runSandboxCache = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "usage: aileron sandbox cache") {
		t.Fatalf("stderr = %q", errb.String())
	}
}
