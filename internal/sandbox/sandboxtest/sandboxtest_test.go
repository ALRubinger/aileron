package sandboxtest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/sandbox/sandboxtest"
)

// --- RecordingExecutor ---

func TestRecordingExecutor_RecordsEnvelope(t *testing.T) {
	r := &sandboxtest.RecordingExecutor{
		Result: sandbox.SpawnResult{ExitCode: 0, Stdout: []byte("ok")},
	}
	env := sandbox.SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
	}
	res, err := r.Spawn(context.Background(), env, sandbox.SpawnLimits{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if string(res.Stdout) != "ok" {
		t.Errorf("Stdout = %q", string(res.Stdout))
	}
	if r.CallCount() != 1 {
		t.Errorf("CallCount = %d, want 1", r.CallCount())
	}
	if r.LastCall().Program != "/usr/bin/git" {
		t.Errorf("LastCall.Program = %q", r.LastCall().Program)
	}
	got := r.Calls()
	if len(got) != 1 {
		t.Errorf("Calls() len = %d, want 1", len(got))
	}
}

func TestRecordingExecutor_HookOverridesScriptedResult(t *testing.T) {
	r := &sandboxtest.RecordingExecutor{
		Result: sandbox.SpawnResult{ExitCode: 0},
		Hook: func(env sandbox.SpawnEnvelope) (sandbox.SpawnResult, error) {
			if env.Argv[1] == "log" {
				return sandbox.SpawnResult{ExitCode: 0, Stdout: []byte("commits")}, nil
			}
			return sandbox.SpawnResult{ExitCode: 0, Stdout: []byte("status")}, nil
		},
	}
	res1, _ := r.Spawn(context.Background(), sandbox.SpawnEnvelope{Argv: []string{"git", "log"}}, sandbox.SpawnLimits{})
	res2, _ := r.Spawn(context.Background(), sandbox.SpawnEnvelope{Argv: []string{"git", "status"}}, sandbox.SpawnLimits{})
	if string(res1.Stdout) != "commits" {
		t.Errorf("res1 = %q", string(res1.Stdout))
	}
	if string(res2.Stdout) != "status" {
		t.Errorf("res2 = %q", string(res2.Stdout))
	}
}

func TestRecordingExecutor_PropagatesError(t *testing.T) {
	r := &sandboxtest.RecordingExecutor{Err: sandbox.ErrSpawnUnavailable}
	_, err := r.Spawn(context.Background(), sandbox.SpawnEnvelope{Program: "/usr/bin/git", Argv: []string{"git"}}, sandbox.SpawnLimits{})
	if !errors.Is(err, sandbox.ErrSpawnUnavailable) {
		t.Errorf("err = %v, want ErrSpawnUnavailable", err)
	}
}

func TestRecordingExecutor_LastCallEmptyWhenUnused(t *testing.T) {
	r := &sandboxtest.RecordingExecutor{}
	if got := r.LastCall(); got.Program != "" {
		t.Errorf("LastCall on empty = %+v", got)
	}
}

func TestRecordingExecutor_ResetClearsState(t *testing.T) {
	r := &sandboxtest.RecordingExecutor{
		Result: sandbox.SpawnResult{ExitCode: 7},
	}
	_, _ = r.Spawn(context.Background(), sandbox.SpawnEnvelope{Argv: []string{"x"}}, sandbox.SpawnLimits{})
	r.Reset()
	if r.CallCount() != 0 {
		t.Errorf("CallCount after Reset = %d", r.CallCount())
	}
	res, _ := r.Spawn(context.Background(), sandbox.SpawnEnvelope{Argv: []string{"x"}}, sandbox.SpawnLimits{})
	if res.ExitCode != 0 {
		t.Errorf("Result was not cleared: %+v", res)
	}
}

// --- FakeBinary ---

func TestFakeBinary_EmitsStdoutAndStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FakeBinary is POSIX-only in v1")
	}
	dir := t.TempDir()
	fb := sandboxtest.FakeBinary{
		Stdout:   "hello\n",
		Stderr:   "warning: something\n",
		ExitCode: 0,
	}
	path, err := fb.Write(dir, "fake-cli")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	cmd := exec.Command(path)
	stdout, runErr := cmd.Output()
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if got := string(stdout); got != "hello\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestFakeBinary_PropagatesNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FakeBinary is POSIX-only in v1")
	}
	dir := t.TempDir()
	fb := sandboxtest.FakeBinary{ExitCode: 7}
	path, err := fb.Write(dir, "fake-cli")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = exec.Command(path).Run()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v", err)
	}
	if exitErr.ExitCode() != 7 {
		t.Errorf("exit = %d, want 7", exitErr.ExitCode())
	}
}

func TestFakeBinary_RecordsArgvAndEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FakeBinary is POSIX-only in v1")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	recPath := filepath.Join(dir, "recording.json")
	fb := sandboxtest.FakeBinary{
		Stdout:     "ok",
		RecordPath: recPath,
	}
	path, err := fb.Write(dir, "fake-cli")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	cmd := exec.Command(path, "log", "--since=2026-01-01")
	cmd.Env = []string{"GIT_AUTHOR_NAME=alr", "GH_TOKEN=secret"}
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	rec, err := sandboxtest.LoadRecording(recPath)
	if err != nil {
		t.Fatalf("LoadRecording: %v", err)
	}
	if !contains(rec.Argv, "log") || !contains(rec.Argv, "--since=2026-01-01") {
		t.Errorf("Argv = %v", rec.Argv)
	}
	if !rec.EnvHas("GIT_AUTHOR_NAME", "alr") {
		t.Errorf("env missing GIT_AUTHOR_NAME=alr; got %v", rec.Env)
	}
	if !rec.EnvHas("GH_TOKEN", "secret") {
		t.Errorf("env missing GH_TOKEN=secret")
	}
	keys := rec.EnvKeys()
	if !contains(keys, "GIT_AUTHOR_NAME") {
		t.Errorf("EnvKeys = %v", keys)
	}
}

func TestLoadRecording_RejectsMissingFile(t *testing.T) {
	if _, err := sandboxtest.LoadRecording("/nonexistent/recording.json"); err == nil {
		t.Error("expected error for missing recording file")
	}
}

func TestLoadRecording_RejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := writeFile(path, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxtest.LoadRecording(path); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestRecording_NilSafe(t *testing.T) {
	var r *sandboxtest.Recording
	if r.EnvHas("X", "Y") {
		t.Error("nil Recording should report no env")
	}
	if got := r.EnvKeys(); got != nil {
		t.Errorf("nil Recording EnvKeys = %v", got)
	}
}

// --- helpers ---

func contains(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
