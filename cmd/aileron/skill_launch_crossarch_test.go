package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// stubCrossArchBoot swaps the single real-Docker call site for a recorder that
// writes stderrText to the stderr writer it is handed and returns runErr, so a
// cross-arch manifest miss (daemon stderr text + a generic exit-125 error) is
// reproducible with no live runtime. It also pins the CLI-version-skew preflight
// to "" and the host operator id to a fixed value so every Run stays Docker-free
// and deterministic, and records the writer it was given so the pass-through
// contract is assertable. The recorded writer is returned via gotStderr.
func stubCrossArchBoot(t *testing.T, stderrText string, runErr error, gotStderr *io.Writer) {
	t.Helper()
	stubBakedCLIVersion(t, "")
	origActor := operatorActorID
	operatorActorID = func() string { return stubHostOperatorID }
	t.Cleanup(func() { operatorActorID = origActor })
	orig := containerRunFlightPlan
	containerRunFlightPlan = func(_ context.Context, _ string, _, stderr io.Writer, _ sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		if gotStderr != nil {
			*gotStderr = stderr
		}
		if stderrText != "" {
			_, _ = io.WriteString(stderr, stderrText)
		}
		return sandboxcontainer.RunResult{}, runErr
	}
	t.Cleanup(func() { containerRunFlightPlan = orig })
}

// stubHostPlatform pins the hostPlatform seam so the cross-arch message names a
// deterministic platform independent of the test host's real architecture.
func stubHostPlatform(t *testing.T, platform string) {
	t.Helper()
	orig := hostPlatform
	hostPlatform = func() string { return platform }
	t.Cleanup(func() { hostPlatform = orig })
}

func crossArchSpec(t *testing.T) runtime.ImageRunSpec {
	t.Helper()
	origStore := skillStoreDir
	skillStoreDir = t.TempDir()
	t.Cleanup(func() { skillStoreDir = origStore })
	return runtime.ImageRunSpec{
		Image:   "registry.example.com/runner:1.4@sha256:abc",
		Name:    "weekly-metrics-digest",
		Version: "1.0.0",
	}
}

// TestCrossArch_ManifestMissRewritesError is the feature happy path: a realistic
// daemon "no matching manifest for …" stderr line plus a generic exit-125 error
// is rewritten into an actionable message that names the host platform and the
// publisher's responsibility, and does not lead with the raw daemon/exit text.
func TestCrossArch_ManifestMissRewritesError(t *testing.T) {
	spec := crossArchSpec(t)
	stubHostPlatform(t, "linux/amd64")
	stubCrossArchBoot(t,
		"docker: no matching manifest for linux/amd64 in the manifest list entries: no match for platform in manifest: not found\n",
		errors.New("exit status 125"), nil)

	_, err := containerImageRunner{}.Run(context.Background(), spec)
	if err == nil {
		t.Fatal("Run: expected an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "linux/amd64") {
		t.Errorf("message does not name the host platform: %q", msg)
	}
	if !strings.Contains(msg, "publisher must publish") {
		t.Errorf("message lacks the publisher-must-publish guidance: %q", msg)
	}
	if !strings.Contains(msg, spec.Image) {
		t.Errorf("message does not name the pinned image: %q", msg)
	}
	if strings.HasPrefix(msg, "no matching manifest") || strings.HasPrefix(msg, "exit status 125") {
		t.Errorf("message leads with the raw daemon/exit text: %q", msg)
	}
	if strings.Contains(msg, "exit status 125") {
		t.Errorf("message should not carry the uninformative exit status: %q", msg)
	}
}

// TestCrossArch_UnrelatedFailureUnchanged proves no false rewrite: an unrelated
// stderr line and an unrelated error fall through to the generic wrap unchanged.
func TestCrossArch_UnrelatedFailureUnchanged(t *testing.T) {
	spec := crossArchSpec(t)
	stubHostPlatform(t, "linux/amd64")
	stubCrossArchBoot(t, "some unrelated daemon noise\n", errors.New("some other failure"), nil)

	_, err := containerImageRunner{}.Run(context.Background(), spec)
	if err == nil {
		t.Fatal("Run: expected an error, got nil")
	}
	want := `skill launch: run pinned image "registry.example.com/runner:1.4@sha256:abc": some other failure`
	if err.Error() != want {
		t.Errorf("generic wrap changed:\n got: %q\nwant: %q", err.Error(), want)
	}
}

// TestCrossArch_ExitCodeAloneDoesNotOverMatch guards against exit-code-only
// matching: an exit-125 failure with no manifest text must NOT be rewritten.
func TestCrossArch_ExitCodeAloneDoesNotOverMatch(t *testing.T) {
	spec := crossArchSpec(t)
	stubHostPlatform(t, "linux/amd64")
	stubCrossArchBoot(t,
		"docker: Error response from daemon: could not select device driver\n",
		errors.New("exit status 125"), nil)

	_, err := containerImageRunner{}.Run(context.Background(), spec)
	if err == nil {
		t.Fatal("Run: expected an error, got nil")
	}
	if strings.Contains(err.Error(), "publisher must publish") {
		t.Errorf("exit-125 without manifest text was wrongly rewritten: %q", err.Error())
	}
	want := `skill launch: run pinned image "registry.example.com/runner:1.4@sha256:abc": exit status 125`
	if err.Error() != want {
		t.Errorf("generic wrap changed:\n got: %q\nwant: %q", err.Error(), want)
	}
}

// TestCrossArch_SignalMatchIsCaseInsensitive confirms the classifier tolerates
// casing and surrounding daemon text, matching on the substring alone.
func TestCrossArch_SignalMatchIsCaseInsensitive(t *testing.T) {
	spec := crossArchSpec(t)
	stubHostPlatform(t, "linux/arm64")
	stubCrossArchBoot(t,
		"DOCKER: NO MATCHING MANIFEST FOR linux/arm64 in the manifest list entries\n",
		errors.New("exit status 125"), nil)

	_, err := containerImageRunner{}.Run(context.Background(), spec)
	if err == nil {
		t.Fatal("Run: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "linux/arm64") || !strings.Contains(err.Error(), "publisher must publish") {
		t.Errorf("case-insensitive signal was not classified: %q", err.Error())
	}
}

// TestCrossArch_StderrPassThroughPreserved confirms the capture tee forwards the
// full stderr text to the writer the runner is handed, so terminal output is
// unaffected regardless of classification.
func TestCrossArch_StderrPassThroughPreserved(t *testing.T) {
	spec := crossArchSpec(t)
	stubHostPlatform(t, "linux/amd64")
	var got io.Writer
	line := "docker: no matching manifest for linux/amd64 in the manifest list entries: not found\n"
	stubCrossArchBoot(t, line, errors.New("exit status 125"), &got)

	// The runner writes to os.Stderr in production; the tee wraps it. We assert
	// the stub received a writer and that writing to it reaches the underlying
	// sink by exercising a fresh capture writer with a buffer sink directly.
	if _, err := (containerImageRunner{}).Run(context.Background(), spec); err == nil {
		t.Fatal("Run: expected an error, got nil")
	}
	if got == nil {
		t.Fatal("runner was not handed a stderr writer")
	}

	// Contract of the tee itself: full pass-through plus a bounded recorded copy.
	var sink bytes.Buffer
	pw := &prefixCaptureWriter{w: &sink, max: crossArchStderrCaptureMax}
	if _, err := io.WriteString(pw, line); err != nil {
		t.Fatalf("tee write: %v", err)
	}
	if sink.String() != line {
		t.Errorf("tee did not pass through full text:\n got: %q\nwant: %q", sink.String(), line)
	}
	if pw.captured() != line {
		t.Errorf("tee did not record the text:\n got: %q\nwant: %q", pw.captured(), line)
	}
}

// TestPrefixCaptureWriter_CapsRecordingButPassesThrough proves the recorded copy
// is bounded at max while the underlying writer still receives every byte.
func TestPrefixCaptureWriter_CapsRecordingButPassesThrough(t *testing.T) {
	var sink bytes.Buffer
	pw := &prefixCaptureWriter{w: &sink, max: 4}
	if _, err := io.WriteString(pw, "abcdefgh"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.WriteString(pw, "ijkl"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := pw.captured(); got != "abcd" {
		t.Errorf("recorded copy not capped at max: got %q want %q", got, "abcd")
	}
	if got := sink.String(); got != "abcdefghijkl" {
		t.Errorf("underlying writer lost bytes: got %q want %q", got, "abcdefghijkl")
	}
}
