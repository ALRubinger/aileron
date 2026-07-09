package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// stubLaunchTTY forces isTTYFn to report the given interactivity for the test.
func stubLaunchTTY(t *testing.T, tty bool) {
	t.Helper()
	orig := isTTYFn
	isTTYFn = func() bool { return tty }
	t.Cleanup(func() { isTTYFn = orig })
}

// stubLaunchWalkerStdin swaps the walker seam for one reading a scripted stdin,
// recording whether it was ever constructed (the launch is interactive) so a
// test can assert the walk ran or was skipped.
func stubLaunchWalkerStdin(t *testing.T, script string) *bool {
	t.Helper()
	constructed := false
	orig := newLaunchInputWalker
	newLaunchInputWalker = func(_ *bufio.Reader, stdout io.Writer) runtime.InputWalker {
		constructed = true
		return launchInputWalker{stdin: bufio.NewReader(strings.NewReader(script)), stdout: stdout}
	}
	t.Cleanup(func() { newLaunchInputWalker = orig })
	return &constructed
}

// usePassthroughImageRunner routes launch through the REAL containerImageRunner
// (zero-value passthrough) so the boot command is recorded via
// containerRunFlightPlan, rather than the fake image runner that bypasses
// serialization.
func usePassthroughImageRunner(t *testing.T) {
	t.Helper()
	orig := newLaunchImageRunner
	newLaunchImageRunner = func() runtime.ImageRunner { return containerImageRunner{} }
	t.Cleanup(func() { newLaunchImageRunner = orig })
}

// TestRunSkillLaunch_TTYWalkFeedsSealedBoot is the contract-level proof the
// guided walk is LIVE on the image-pinned mainline (#2063): on a faked TTY the
// host-side walk runs before container boot. An Enter-accepted default records
// NO override, so the recorded boot command carries no --input for window_days
// and the sealed plan resolves the declared default natively inside the
// container, in parity with --accept-defaults. The walk running is proven by the
// walked flag; the absence of a serialized accepted-default proves it does not
// round-trip the default through --input.
func TestRunSkillLaunch_TTYWalkFeedsSealedBoot(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})
	usePassthroughImageRunner(t)

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubLaunchTTY(t, true)
	walked := stubLaunchWalkerStdin(t, "\n") // Enter-accept the window_days default

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if !*walked {
		t.Fatal("an interactive TTY launch must run the guided walk")
	}
	joined := strings.Join(got.Command, " ")
	if strings.Contains(joined, "window_days") {
		t.Errorf("an Enter-accepted default must not be serialized onto the boot command (it resolves natively inside the container), got %v", got.Command)
	}
}

// bootInputArgs runs a launch through the passthrough image runner and returns
// the --input name=value pairs the recorded boot command carries, so a test can
// compare how two launch modes serialize inputs onto the sealed-image re-entry.
func bootInputArgs(t *testing.T, args []string, script string, tty bool) []string {
	t.Helper()
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})
	usePassthroughImageRunner(t)

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubLaunchTTY(t, tty)
	stubLaunchWalkerStdin(t, script)

	var stdout, stderr bytes.Buffer
	if code := runSkillLaunch(args, &stdout, &stderr); code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	var inputs []string
	for i := 0; i+1 < len(got.Command); i++ {
		if got.Command[i] == "--input" {
			inputs = append(inputs, got.Command[i+1])
		}
	}
	return inputs
}

// TestRunSkillLaunch_EnterDefaultParityWithAcceptDefaults is the parity
// regression for #2063: accepting a declared default with Enter on an
// interactive TTY and launching with --accept-defaults must serialize inputs
// onto the sealed-image boot command identically, so both modes resolve the same
// value and type for the accepted default natively inside the container. Before
// the fix the interactive Enter round-tripped window_days through
// --input window_days=7 (re-parsed as a string), while --accept-defaults left it
// to resolve as the native int, diverging by launch mode on the image path.
func TestRunSkillLaunch_EnterDefaultParityWithAcceptDefaults(t *testing.T) {
	outDir := t.TempDir()
	interactive := bootInputArgs(t,
		[]string{"--out-dir", outDir, "weekly-metrics-digest"},
		"\n", // Enter-accept the window_days default on a TTY
		true)
	defaulted := bootInputArgs(t,
		[]string{"--accept-defaults", "--out-dir", outDir, "weekly-metrics-digest"},
		"should-not-be-read\n",
		true)
	if len(interactive) != 0 {
		t.Errorf("interactive Enter-accept must serialize no --input for an accepted default, got %v", interactive)
	}
	if !reflect.DeepEqual(interactive, defaulted) {
		t.Errorf("interactive Enter-accept and --accept-defaults must serialize inputs identically on the image path: interactive=%v, --accept-defaults=%v", interactive, defaulted)
	}
}

// TestRunSkillLaunch_AcceptDefaultsSkipsWalk proves --accept-defaults reproduces
// today's silent-default one-shot launch even on a TTY: the walk never runs, no
// prompt is emitted, and the boot command carries no walked --input.
func TestRunSkillLaunch_AcceptDefaultsSkipsWalk(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})
	usePassthroughImageRunner(t)

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubLaunchTTY(t, true)
	walked := stubLaunchWalkerStdin(t, "should-not-be-read\n")

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--accept-defaults", "--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if *walked {
		t.Fatal("--accept-defaults must skip the guided walk even on a TTY")
	}
	for _, a := range got.Command {
		if a == "--input" {
			t.Errorf("--accept-defaults must emit no walked --input flags, got %v", got.Command)
		}
	}
}

// TestRunSkillLaunch_NonTTYSkipsWalk proves a non-TTY launch (piped stdin, CI)
// implies --accept-defaults: the walk never runs.
func TestRunSkillLaunch_NonTTYSkipsWalk(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})
	usePassthroughImageRunner(t)

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubLaunchTTY(t, false)
	walked := stubLaunchWalkerStdin(t, "should-not-be-read\n")

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if *walked {
		t.Fatal("a non-TTY launch must not run the guided walk")
	}
}

// freezeRequiredInputForLaunch installs a no-environment variant of the worked
// example with window_days's default removed, so window_days becomes a required
// literal (no default) resolved on the in-process path. The launch fails fast if
// the input is neither overridden nor prompted.
func freezeRequiredInputForLaunch(t *testing.T, storeDir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForTest(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	stripped := stripEnvironmentBlock(t, string(raw))
	// Remove the llm-seam step so this fixture isolates the input-resolution
	// fail-fast path. The worked example carries a `summarize` llm-seam step, and
	// the runtime's surface gate (#2102) fails a seam plan closed on a non-agent
	// surface BEFORE input resolution runs. This test drives the required-input
	// path, not the seam gate, so it removes the seam (and rewires the dangling
	// binding) to keep the plan deterministic.
	stripped = stripSeamStepRewireBinding(t, stripped)
	// Remove the window_days default so it becomes a required-no-default literal.
	required := strings.Replace(stripped, "\n        default: 7", "", 1)
	if required == stripped {
		t.Fatal("failed to remove the window_days default from the worked example")
	}
	dir := filepath.Join(storeDir, "weekly-metrics-digest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(required), 0o644); err != nil {
		t.Fatal(err)
	}
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)
	var out, errOut bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "weekly-metrics-digest"}, &out, &errOut); code != 0 {
		t.Fatalf("freeze required-input variant failed: %s", errOut.String())
	}
}

// TestRunSkillLaunch_AcceptDefaultsFailFastOnRequired is the round-2 P0
// regression: a required-with-no-default literal under --accept-defaults on a
// TTY must fail fast WITHOUT prompting (both interactive seams nil, so
// resolveLiteral fail-fasts), rather than blocking on a prompt or silently
// resolving to empty.
func TestRunSkillLaunch_AcceptDefaultsFailFastOnRequired(t *testing.T) {
	storeDir := withTempStore(t)
	freezeRequiredInputForLaunch(t, storeDir)
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})
	stubLaunchTTY(t, true)
	walked := stubLaunchWalkerStdin(t, "would-satisfy-it\n")

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--accept-defaults", "--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("a required-no-default input under --accept-defaults must fail fast, stdout=%s", stdout.String())
	}
	if *walked {
		t.Fatal("--accept-defaults must not prompt for the required input")
	}
	if !strings.Contains(stderr.String(), "is required") {
		t.Errorf("failure must name the missing required input, stderr=%s", stderr.String())
	}
}
