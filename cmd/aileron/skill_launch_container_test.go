package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

// stubHostOperatorID is the deterministic host-resolved operator identity the
// container Run tests stamp via the operatorActorID seam, so the injected
// AILERON_OPERATOR_ID env is a fixed value independent of the host user/host.
const stubHostOperatorID = "carol@host-workstation"

// stubContainerBoot swaps the single real-Docker call site for a recorder so the
// container image runner's spec-construction is testable with no live runtime.
// It also stubs the CLI-version-skew preflight to "" so every Run test stays
// Docker-free and warning-silent by default (skew tests override it via
// stubBakedCLIVersion), and stubs operatorActorID to stubHostOperatorID so the
// host-resolved AILERON_OPERATOR_ID injected into the boot env (#1881) is
// deterministic.
func stubContainerBoot(t *testing.T, capture *sandboxcontainer.RunOptions, err error) {
	t.Helper()
	stubBakedCLIVersion(t, "")
	origActor := operatorActorID
	operatorActorID = func() string { return stubHostOperatorID }
	t.Cleanup(func() { operatorActorID = origActor })
	orig := containerRunFlightPlan
	containerRunFlightPlan = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		*capture = opts
		return sandboxcontainer.RunResult{}, err
	}
	t.Cleanup(func() { containerRunFlightPlan = orig })
}

// stubBakedCLIVersion swaps the CLI-version inspect seam for a fake returning
// value, recording the image it was asked to inspect. This keeps the preflight
// off `docker image inspect` in unit tests and lets skew tests assert the
// three cases (match, mismatch, missing label) through Run. It returns a pointer
// to the recorded image so tests can assert the pin was passed, never re-resolved.
func stubBakedCLIVersion(t *testing.T, value string) *string {
	t.Helper()
	var inspected string
	orig := containerBakedCLIVersion
	containerBakedCLIVersion = func(_ context.Context, _ string, image string) string {
		inspected = image
		return value
	}
	t.Cleanup(func() { containerBakedCLIVersion = orig })
	return &inspected
}

func TestContainerImageRunner_BootsExactImageWithMounts(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	outDir := t.TempDir()
	spec := runtime.ImageRunSpec{
		Image:   "registry.example.com/runner:1.4@sha256:abc",
		Name:    "weekly-metrics-digest",
		Version: "1.0.0",
		Inputs:  runtime.LaunchArgs{"window_days": "30"},
		OutDir:  outDir,
	}
	res, err := containerImageRunner{}.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The booted image must be the exact pin, never re-resolved.
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
	// The store is mounted read-only; the out-dir writable.
	var storeRO, outRW bool
	for _, v := range got.Volumes {
		if v.Source == storeDir && v.ReadOnly {
			storeRO = true
		}
		if v.Source == outDir && !v.ReadOnly {
			outRW = true
		}
	}
	if !storeRO {
		t.Errorf("frozen store must be mounted read-only, got %+v", got.Volumes)
	}
	if !outRW {
		t.Errorf("out-dir must be mounted writable, got %+v", got.Volumes)
	}
	// The in-container command re-enters aileron against the mounted unit.
	joined := strings.Join(got.Command, " ")
	if !strings.Contains(joined, "skill launch weekly-metrics-digest") {
		t.Errorf("command = %v, want an in-container skill launch of the unit", got.Command)
	}
	if !strings.Contains(joined, "--version 1.0.0") {
		t.Errorf("command must pin the version: %v", got.Command)
	}
	// The inner binary must be pointed at the bind-mounted store, not the empty
	// default store inside the image.
	if !strings.Contains(joined, "--store-dir /aileron/skills") {
		t.Errorf("command must point the inner binary at the mounted store: %v", got.Command)
	}
	// The container name carries a run-unique suffix so concurrent launches of
	// the same unit never collide.
	if got.Name == flightPlanContainerName(spec) {
		t.Error("container name must include a run-unique suffix, not a fully deterministic value")
	}
	if !strings.HasPrefix(got.Name, "aileron-flightplan-weekly-metrics-digest-1.0.0-") {
		t.Errorf("container name = %q, want the stable base prefix plus a suffix", got.Name)
	}
	// The result echoes the resolved inputs in v1.
	if res.ResolvedInputs["window_days"] != "30" {
		t.Errorf("result inputs = %v, want the launch inputs echoed", res.ResolvedInputs)
	}
}

// indexOf returns the position of want in args, or -1 if absent. It is a small
// helper for asserting the ordered emission of --input flags on the re-entry.
func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// TestContainerImageRunner_ReEmitsInputOverrides is the #1802 regression proof:
// launch input overrides carried in ImageRunSpec.Inputs must be re-emitted as
// --input name=value on the in-container re-entry, in sorted-key order, so the
// inner binary applies them rather than silently resolving defaults. Before the
// fix the command carried no --input flags and this asserts they are present,
// value-exact, and deterministically ordered.
func TestContainerImageRunner_ReEmitsInputOverrides(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
		Inputs: runtime.LaunchArgs{
			"window_days": "30",
			"format":      "csv",
			"account":     "acme",
		},
	}
	if _, err := (containerImageRunner{}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every override is re-emitted as an adjacent --input name=value pair.
	joined := strings.Join(got.Command, " ")
	for _, want := range []string{
		"--input account=acme",
		"--input format=csv",
		"--input window_days=30",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("command %v must carry %q", got.Command, want)
		}
	}

	// The emission is deterministically ordered by sorted key: account < format <
	// window_days, so the value flags appear in that relative order.
	iAccount := indexOf(got.Command, "account=acme")
	iFormat := indexOf(got.Command, "format=csv")
	iWindow := indexOf(got.Command, "window_days=30")
	if iAccount == -1 || iFormat == -1 || iWindow == -1 {
		t.Fatalf("all overrides must be present in the command: %v", got.Command)
	}
	if !(iAccount < iFormat && iFormat < iWindow) {
		t.Errorf("overrides must be emitted in sorted-key order, got command %v", got.Command)
	}
}

// TestContainerImageRunner_NoOverridesLeavesCommandUnchanged proves a launch with
// no input overrides emits no --input flags, so the boot command is unchanged for
// the common no-override case (#1802).
func TestContainerImageRunner_NoOverridesLeavesCommandUnchanged(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	spec := runtime.ImageRunSpec{
		Image:   "registry.example.com/runner:1.4@sha256:abc",
		Name:    "weekly-metrics-digest",
		Version: "1.0.0",
	}
	if _, err := (containerImageRunner{}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, a := range got.Command {
		if a == "--input" {
			t.Errorf("a no-override launch must emit no --input flags, got %v", got.Command)
		}
	}
	if strings.Join(got.Command, " ") != "aileron skill launch weekly-metrics-digest --store-dir /aileron/skills --version 1.0.0" {
		t.Errorf("no-override command = %v, want the base re-entry unchanged", got.Command)
	}
}

// TestContainerImageRunner_TypedOverrideSerializesViaSprintf proves a non-string
// value in the map[string]any seam type (a typed default the host-side input walk
// injects on Enter-accept, #2063) serializes to --input k=<%v> and the boot
// FIRES. The %v round-trip is faithful: the inner re-entry re-parses it as a
// string and every downstream check compares via %v, so a typed default and its
// string form are indistinguishable to the plan. This replaces the earlier
// refuse-non-string guard, which predated the walk.
func TestContainerImageRunner_TypedOverrideSerializesViaSprintf(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
		Inputs: runtime.LaunchArgs{
			"window_days": 7,      // number: an Enter-accepted typed default
			"enabled":     true,   // bool: another native typed value
			"account":     "acme", // string: the CLI-override case still works
		},
	}
	if _, err := (containerImageRunner{}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := strings.Join(got.Command, " ")
	for _, want := range []string{
		"--input account=acme",
		"--input enabled=true",
		"--input window_days=7",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("command %v must carry %q (typed value serialized via %%v)", got.Command, want)
		}
	}
}

// fakeImageDaemonEnv is a canned imageDaemonEnv for the runner tests: it returns
// the recorded env map and ok flag so the runner's RunOptions.Env wiring is
// asserted with no live daemon and no Docker.
type fakeImageDaemonEnv struct {
	env        map[string]string
	ok         bool
	gotRuntime string
	callCount  int
}

func (f *fakeImageDaemonEnv) Env(runtimeName string) (map[string]string, bool) {
	f.callCount++
	f.gotRuntime = runtimeName
	return f.env, f.ok
}

// TestContainerImageRunner_InjectsDaemonEnv proves that when a daemon-env
// resolver is wired and resolves, the booted container carries the host daemon
// coordinates so the re-entered in-container launch reaches the host daemon
// action + audit boundary (#1759).
func TestContainerImageRunner_InjectsDaemonEnv(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	fake := &fakeImageDaemonEnv{
		env: map[string]string{
			"AILERON_API_URL": "http://host.docker.internal:48123/v1",
			"AILERON_TOKEN":   "daemon-token",
		},
		ok: true,
	}
	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
	if _, err := (containerImageRunner{daemonEnv: fake}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.callCount != 1 {
		t.Fatalf("resolver called %d times, want 1", fake.callCount)
	}
	// The resolver is passed the resolved runtime name so it can host-rewrite the
	// daemon URL for that runtime.
	if fake.gotRuntime == "" {
		t.Error("resolver must be passed the resolved runtime name")
	}
	if got.Env["AILERON_API_URL"] != "http://host.docker.internal:48123/v1" {
		t.Errorf("AILERON_API_URL = %q, want the host-rewritten /v1 daemon URL", got.Env["AILERON_API_URL"])
	}
	if got.Env["AILERON_TOKEN"] != "daemon-token" {
		t.Errorf("AILERON_TOKEN = %q, want the daemon token", got.Env["AILERON_TOKEN"])
	}
	if got.Env[envSkillImageBooted] != "1" {
		t.Errorf("%s = %q, want the boot sentinel alongside the daemon env", envSkillImageBooted, got.Env[envSkillImageBooted])
	}
	// #1881: the host-resolved operator identity is injected into the boot env
	// so the in-container CLI stamps the real human on its audit records rather
	// than the image's fixed non-root user + ephemeral container id.
	if got.Env["AILERON_OPERATOR_ID"] != stubHostOperatorID {
		t.Errorf("AILERON_OPERATOR_ID = %q, want the host-resolved operator id %q", got.Env["AILERON_OPERATOR_ID"], stubHostOperatorID)
	}
}

// TestContainerImageRunner_PassthroughWhenNoDaemonConfig proves that when the
// resolver reports ok=false (no daemon config), the boot carries no injected env
// so a no-daemon launch is unaffected (#1759).
func TestContainerImageRunner_PassthroughWhenNoDaemonConfig(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	fake := &fakeImageDaemonEnv{ok: false}
	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
	if _, err := (containerImageRunner{daemonEnv: fake}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The boot sentinel and the host-resolved operator id (#1881), but no daemon
	// coordinates, on the passthrough boot.
	if len(got.Env) != 2 || got.Env[envSkillImageBooted] != "1" || got.Env["AILERON_OPERATOR_ID"] != stubHostOperatorID {
		t.Errorf("RunOptions.Env = %v, want only the boot sentinel + operator id on the passthrough (ok=false) boot", got.Env)
	}
}

// TestContainerImageRunner_ZeroValueInjectsNoEnv proves the zero-value runner
// (daemonEnv == nil) injects no daemon env, so the CLI unit tests stay
// deterministic irrespective of any live ~/.aileron daemon (#1759). The boot
// sentinel is still present: every whole-plan boot marks the re-entry.
func TestContainerImageRunner_ZeroValueInjectsNoEnv(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
	if _, err := (containerImageRunner{}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Env) != 2 || got.Env[envSkillImageBooted] != "1" || got.Env["AILERON_OPERATOR_ID"] != stubHostOperatorID {
		t.Errorf("RunOptions.Env = %v, want only the boot sentinel + operator id on the zero-value runner", got.Env)
	}
}

// TestNewLaunchImageRunner_WiresDaemonEnvAndProxy proves the production seam
// wires both the daemon-backed env resolver (#1759) and the daemon-backed plan
// proxy bootstrapper (#1828) so production boots carry the host daemon
// coordinates AND the proxy/CA/placeholder enrichment.
func TestNewLaunchImageRunner_WiresDaemonEnvAndProxy(t *testing.T) {
	runner, ok := newLaunchImageRunner().(containerImageRunner)
	if !ok {
		t.Fatalf("newLaunchImageRunner returned %T, want containerImageRunner", newLaunchImageRunner())
	}
	if _, ok := runner.daemonEnv.(daemonImageEnv); !ok {
		t.Errorf("daemonEnv = %T, want daemonImageEnv (the production resolver)", runner.daemonEnv)
	}
	if _, ok := runner.proxy.(daemonPlanProxyBootstrapper); !ok {
		t.Errorf("proxy = %T, want daemonPlanProxyBootstrapper (the production bootstrapper)", runner.proxy)
	}
	if runner.diag != nil {
		t.Errorf("newLaunchImageRunner diag = %v, want nil (os.Stderr in production)", runner.diag)
	}
}

// TestNewLaunchToolStepRunner_ZeroValue proves the production tool-step
// runner seam yields the in-container subprocess runner (#1829).
func TestNewLaunchToolStepRunner_ZeroValue(t *testing.T) {
	if _, ok := newLaunchToolStepRunner().(inContainerToolStepRunner); !ok {
		t.Fatalf("newLaunchToolStepRunner returned %T, want inContainerToolStepRunner", newLaunchToolStepRunner())
	}
}

// fakePlanProxyBootstrapper is a test double for planProxyBootstrapper. It
// records that Prepare/cleanup ran and returns a canned enrichment so the
// enriched boot RunOptions shape is asserted with no live daemon and no Docker.
type fakePlanProxyBootstrapper struct {
	env         map[string]string
	mount       sandboxcontainer.Volume
	ok          bool
	err         error
	prepared    bool
	cleaned     bool
	gotRuntime  string
	gotImage    string
	gotPlanName string
}

func (f *fakePlanProxyBootstrapper) Prepare(_ context.Context, runtimeName, image, planName string) (planProxyBootstrap, func(), bool, error) {
	f.prepared = true
	f.gotRuntime = runtimeName
	f.gotImage = image
	f.gotPlanName = planName
	if f.err != nil {
		return planProxyBootstrap{}, func() { f.cleaned = true }, false, f.err
	}
	return planProxyBootstrap{Env: f.env, Mount: f.mount}, func() { f.cleaned = true }, f.ok, nil
}

// TestContainerImageRunner_ProxyEnrichesBoot proves that when a proxy
// bootstrapper is wired and resolves, the whole-plan boot appends the read-only
// CA mount and merges the proxy/CA/placeholder env alongside the boot sentinel
// and daemon env (#1828). The distinct key sets never clobber each other, and
// cleanup runs after the synchronous run.
func TestContainerImageRunner_ProxyEnrichesBoot(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	daemon := &fakeImageDaemonEnv{
		env: map[string]string{
			"AILERON_API_URL": "http://host.docker.internal:48123/v1",
			"AILERON_TOKEN":   "daemon-token",
		},
		ok: true,
	}
	proxy := &fakePlanProxyBootstrapper{
		ok: true,
		env: map[string]string{
			"HTTPS_PROXY":       "http://sess:tok@host.docker.internal:48123",
			"https_proxy":       "http://sess:tok@host.docker.internal:48123",
			"NO_PROXY":          "localhost,127.0.0.1,::1,host.docker.internal",
			"no_proxy":          "localhost,127.0.0.1,::1,host.docker.internal",
			"AWS_CA_BUNDLE":     "/etc/aileron/proxy/ca.pem",
			"AWS_ACCESS_KEY_ID": "AKIAIOSFODNN7PLACEHLDR",
			"GH_TOKEN":          "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA",
		},
		mount: sandboxcontainer.Volume{Source: "/host/ca.pem", Target: "/etc/aileron/proxy/ca.pem", ReadOnly: true},
	}
	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
	if _, err := (containerImageRunner{daemonEnv: daemon, proxy: proxy}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !proxy.prepared {
		t.Error("Prepare must be called when a proxy bootstrapper is wired")
	}
	if proxy.gotImage != spec.Image {
		t.Errorf("Prepare image = %q, want the spec image", proxy.gotImage)
	}
	if proxy.gotPlanName != spec.Name {
		t.Errorf("Prepare planName = %q, want the spec name", proxy.gotPlanName)
	}
	if !proxy.cleaned {
		t.Error("cleanup must run after the synchronous boot")
	}

	// The CA mount is appended read-only at the well-known in-container CA path.
	var caMountRO bool
	for _, v := range got.Volumes {
		if v.Target == "/etc/aileron/proxy/ca.pem" && v.ReadOnly {
			caMountRO = true
		}
	}
	if !caMountRO {
		t.Errorf("CA must be mounted read-only at /etc/aileron/proxy/ca.pem, got %+v", got.Volumes)
	}

	// The proxy env is merged in alongside the sentinel + daemon env; the three
	// key sets are disjoint, so none clobbers another.
	if got.Env[envSkillImageBooted] != "1" {
		t.Errorf("%s = %q, want the boot sentinel preserved after the proxy merge", envSkillImageBooted, got.Env[envSkillImageBooted])
	}
	if got.Env["AILERON_API_URL"] != "http://host.docker.internal:48123/v1" || got.Env["AILERON_TOKEN"] != "daemon-token" {
		t.Errorf("daemon env must survive the proxy merge, got API_URL=%q TOKEN=%q", got.Env["AILERON_API_URL"], got.Env["AILERON_TOKEN"])
	}
	if !strings.Contains(got.Env["HTTPS_PROXY"], "host.docker.internal") {
		t.Errorf("HTTPS_PROXY = %q, want the proxy URL", got.Env["HTTPS_PROXY"])
	}
	if got.Env["NO_PROXY"] != "localhost,127.0.0.1,::1,host.docker.internal" {
		t.Errorf("NO_PROXY = %q, want the loopback+daemon bypass list", got.Env["NO_PROXY"])
	}
	if got.Env["AWS_CA_BUNDLE"] != "/etc/aileron/proxy/ca.pem" {
		t.Errorf("AWS_CA_BUNDLE = %q, want the container CA path", got.Env["AWS_CA_BUNDLE"])
	}
	if got.Env["AWS_ACCESS_KEY_ID"] != "AKIAIOSFODNN7PLACEHLDR" || got.Env["GH_TOKEN"] != "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("placeholder creds missing from the merged boot env: %+v", got.Env)
	}
	if got.Env["AILERON_OPERATOR_ID"] != stubHostOperatorID {
		t.Errorf("AILERON_OPERATOR_ID = %q, want the host-resolved operator id preserved through the proxy merge", got.Env["AILERON_OPERATOR_ID"])
	}
}

// TestContainerImageRunner_ProxyPassthroughWhenNotOK proves a bootstrapper
// returning ok=false leaves the boot un-enriched: no CA mount, only the boot
// sentinel on the env (#1828).
func TestContainerImageRunner_ProxyPassthroughWhenNotOK(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	proxy := &fakePlanProxyBootstrapper{ok: false}
	spec := runtime.ImageRunSpec{Image: "img@sha256:abc", Name: "p"}
	if _, err := (containerImageRunner{proxy: proxy}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !proxy.prepared {
		t.Error("Prepare must still be consulted")
	}
	for _, v := range got.Volumes {
		if v.Target == "/etc/aileron/proxy/ca.pem" {
			t.Errorf("passthrough boot must not mount a CA, got %+v", got.Volumes)
		}
	}
	if len(got.Env) != 2 || got.Env[envSkillImageBooted] != "1" || got.Env["AILERON_OPERATOR_ID"] != stubHostOperatorID {
		t.Errorf("RunOptions.Env = %v, want only the boot sentinel + operator id on the passthrough (ok=false) boot", got.Env)
	}
}

// TestContainerImageRunner_ProxyPrepareErrorFailsClosed proves a Prepare error
// (daemon config resolved but the CA could not be written, or a malformed
// metadata convention) fails the boot rather than silently egressing
// un-proxied, and runs the returned cleanup (#1828).
func TestContainerImageRunner_ProxyPrepareErrorFailsClosed(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	booted := false
	orig := containerRunFlightPlan
	containerRunFlightPlan = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		booted = true
		got = opts
		return sandboxcontainer.RunResult{}, nil
	}
	t.Cleanup(func() { containerRunFlightPlan = orig })

	proxy := &fakePlanProxyBootstrapper{err: errors.New("ca write failed")}
	_, err := (containerImageRunner{proxy: proxy}).Run(context.Background(), runtime.ImageRunSpec{Image: "img@sha256:abc", Name: "p"})
	if err == nil || !strings.Contains(err.Error(), "ca write failed") {
		t.Fatalf("Prepare error must fail the boot closed, got %v", err)
	}
	if booted {
		t.Errorf("the image must NOT boot after a fail-closed Prepare error, got %+v", got)
	}
	if !proxy.cleaned {
		t.Error("the returned cleanup must run on the error path so a partial CA never leaks")
	}
}

// TestContainerImageRunner_RejectsReservedBootKeyFromBootstrap proves that a
// bootstrap/placeholder env declaring one of the reserved boot keys (the daemon
// coordinates AILERON_TOKEN / AILERON_API_URL, the host-resolved
// AILERON_OPERATOR_ID (#1881), or the AILERON_SKILL_IMAGE_BOOTED sentinel) fails
// the boot closed with an error naming the offending variable, rather than
// letting the merge clobber the authoritative daemon token/URL, the operator
// identity, or the re-entry sentinel (#1828). An image-declared credential placeholder is
// the realistic source of such a key: PlaceholderEnv only rejects
// convention-vs-convention collisions, so a placeholder that mis-declares a
// reserved key would otherwise reach the merge site. The boot must NOT run.
func TestContainerImageRunner_RejectsReservedBootKeyFromBootstrap(t *testing.T) {
	for _, reserved := range []string{"AILERON_TOKEN", "AILERON_API_URL", "AILERON_OPERATOR_ID", envSkillImageBooted} {
		reserved := reserved
		t.Run(reserved, func(t *testing.T) {
			storeDir := t.TempDir()
			origStore := skillStoreDir
			skillStoreDir = storeDir
			t.Cleanup(func() { skillStoreDir = origStore })

			var got sandboxcontainer.RunOptions
			booted := false
			orig := containerRunFlightPlan
			containerRunFlightPlan = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
				booted = true
				got = opts
				return sandboxcontainer.RunResult{}, nil
			}
			t.Cleanup(func() { containerRunFlightPlan = orig })

			daemon := &fakeImageDaemonEnv{
				env: map[string]string{
					"AILERON_API_URL": "http://host.docker.internal:48123/v1",
					"AILERON_TOKEN":   "daemon-token",
				},
				ok: true,
			}
			// A placeholder union that mis-declares a reserved boot key.
			proxy := &fakePlanProxyBootstrapper{
				ok: true,
				env: map[string]string{
					"HTTPS_PROXY": "http://sess:tok@host.docker.internal:48123",
					reserved:      "poisoned-by-placeholder",
				},
			}
			spec := runtime.ImageRunSpec{Image: "registry.example.com/runner@sha256:abc", Name: "weekly-metrics-digest"}
			_, err := (containerImageRunner{daemonEnv: daemon, proxy: proxy}).Run(context.Background(), spec)
			if err == nil {
				t.Fatalf("a bootstrap env declaring reserved key %q must fail the boot closed", reserved)
			}
			if !strings.Contains(err.Error(), reserved) {
				t.Errorf("error must name the offending variable %q, got %v", reserved, err)
			}
			if booted {
				t.Errorf("the image must NOT boot when a reserved boot key is present, got %+v", got)
			}
			if !proxy.cleaned {
				t.Error("the returned cleanup must run on the reserved-key rejection path so a partial CA never leaks")
			}
		})
	}
}

// --- CLI-version skew preflight (#1809) ---

// newSkewTestSpec returns a minimal spec with the store dir pointed at a temp
// dir so Run reaches the boot without touching the real store.
func newSkewTestSpec(t *testing.T) runtime.ImageRunSpec {
	t.Helper()
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })
	return runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
}

// TestContainerImageRunner_SkewWarnsOnMismatch proves a baked CLI version that
// differs from the host version prints one stderr warning naming both versions
// and the image, still boots the exact pin, and returns no error.
func TestContainerImageRunner_SkewWarnsOnMismatch(t *testing.T) {
	spec := newSkewTestSpec(t)
	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	inspected := stubBakedCLIVersion(t, "9.9.9")

	var buf bytes.Buffer
	if _, err := (containerImageRunner{diag: &buf}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "warning:") {
		t.Errorf("expected a stderr warning, got %q", out)
	}
	if !strings.Contains(out, "9.9.9") {
		t.Errorf("warning must name the baked version 9.9.9: %q", out)
	}
	if !strings.Contains(out, version.Version) {
		t.Errorf("warning must name the host version %q: %q", version.Version, out)
	}
	if !strings.Contains(out, spec.Image) {
		t.Errorf("warning must name the image %q: %q", spec.Image, out)
	}
	// The launch proceeds with the exact pin regardless of skew.
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
	// The preflight inspects the pin, never a re-resolved image.
	if *inspected != spec.Image {
		t.Errorf("preflight inspected %q, want the pin %q", *inspected, spec.Image)
	}
}

// TestContainerImageRunner_SkewSilentOnMatch proves a baked CLI version equal to
// the host version emits nothing (stdout stays byte-identical, no OK line) and
// still boots the pin.
func TestContainerImageRunner_SkewSilentOnMatch(t *testing.T) {
	spec := newSkewTestSpec(t)
	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubBakedCLIVersion(t, version.Version)

	var buf bytes.Buffer
	if _, err := (containerImageRunner{diag: &buf}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("matching version must be silent, got %q", buf.String())
	}
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
}

// TestContainerImageRunner_SkewSilentOnMissingLabel proves an unlabeled or
// uninspectable image (baked == "") stays silent because a custom image's
// contract is the operator's under ADR-0027, and still boots.
func TestContainerImageRunner_SkewSilentOnMissingLabel(t *testing.T) {
	spec := newSkewTestSpec(t)
	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubBakedCLIVersion(t, "")

	var buf bytes.Buffer
	if _, err := (containerImageRunner{diag: &buf}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("missing label must be silent, got %q", buf.String())
	}
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
}

// TestContainerImageRunner_DiagDefaultsToStderr proves the zero-value diag field
// resolves to os.Stderr in production wiring, so a nil diag never routes the
// warning to a discarded writer.
func TestContainerImageRunner_DiagDefaultsToStderr(t *testing.T) {
	if got := (containerImageRunner{}).diagWriter(); got != os.Stderr {
		t.Errorf("zero-value diag writer = %v, want os.Stderr", got)
	}
	var buf bytes.Buffer
	if got := (containerImageRunner{diag: &buf}).diagWriter(); got != &buf {
		t.Errorf("injected diag writer = %v, want the injected buffer", got)
	}
}

// TestReportBakedCLIVersion covers the skew helper's three-way contract directly.
func TestReportBakedCLIVersion(t *testing.T) {
	cases := []struct {
		name       string
		baked      string
		host       string
		wantOutput bool
	}{
		{name: "empty is silent", baked: "", host: "0.0.5", wantOutput: false},
		{name: "match is silent", baked: "0.0.5", host: "0.0.5", wantOutput: false},
		{name: "mismatch warns", baked: "9.9.9", host: "0.0.5", wantOutput: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			reportBakedCLIVersion(&buf, "img:test", tc.baked, tc.host)
			if got := buf.Len() > 0; got != tc.wantOutput {
				t.Fatalf("output presence = %v, want %v (got %q)", got, tc.wantOutput, buf.String())
			}
		})
	}
}

func TestContainerImageRunner_EmptyImageErrors(t *testing.T) {
	_, err := containerImageRunner{}.Run(context.Background(), runtime.ImageRunSpec{})
	if err == nil {
		t.Fatal("an empty image must error before any boot")
	}
}

func TestContainerImageRunner_BootFailureSurfaces(t *testing.T) {
	origStore := skillStoreDir
	skillStoreDir = t.TempDir()
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, errors.New("boot exploded"))

	_, err := containerImageRunner{}.Run(context.Background(), runtime.ImageRunSpec{
		Image: "img@sha256:abc",
		Name:  "unit",
	})
	if err == nil || !strings.Contains(err.Error(), "boot exploded") {
		t.Fatalf("boot failure must surface, got %v", err)
	}
}

// TestNewLaunchImageDigestResolver_WiresProductionResolver proves the
// production wiring seam yields the container-backed resolver (#1863).
func TestNewLaunchImageDigestResolver_WiresProductionResolver(t *testing.T) {
	if _, ok := newLaunchImageDigestResolver().(containerImageDigestResolver); !ok {
		t.Fatalf("newLaunchImageDigestResolver returned %T, want containerImageDigestResolver", newLaunchImageDigestResolver())
	}
}

// TestContainerImageDigestResolver_DelegatesToInspector proves the production
// resolver delegates to imageInspector.localImageContentDigest (the SAME config
// content-digest computation that produced the pin's digest at freeze time),
// returning the serialization-agnostic content digest for a locally-built tag.
// The compare the runtime guard performs is therefore apples-to-apples by
// construction (#1863).
func TestContainerImageDigestResolver_DelegatesToInspector(t *testing.T) {
	tag := "aileron/sandbox-tools:0123456789abcdef"
	inspect := dockerInspectJSON(t, "booted")
	want := wantContentDigestFromDocker(t, inspect)
	fr := &fakeRunner{outputs: map[string]string{
		`image inspect --format {{json .}} ` + tag: inspect + "\n",
	}}
	withFakeInspector(t, fr)

	got, err := containerImageDigestResolver{}.Resolve(context.Background(), tag)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Errorf("resolved digest = %q, want the config content digest %q", got, want)
	}
}

// TestContainerImageDigestResolver_InspectorError proves an inspector-
// construction failure surfaces from Resolve (the runtime guard then fails the
// boot closed, #1863).
func TestContainerImageDigestResolver_InspectorError(t *testing.T) {
	orig := newImageInspector
	newImageInspector = func() (imageInspector, error) {
		return imageInspector{}, errTestInspect
	}
	t.Cleanup(func() { newImageInspector = orig })
	if _, err := (containerImageDigestResolver{}).Resolve(context.Background(), "aileron/sandbox-tools:x"); err == nil {
		t.Error("an inspector-construction error must surface from Resolve")
	}
}
