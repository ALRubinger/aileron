package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/composition"
)

// fakeProvisioner is an in-memory toolchainProvisioner: it returns canned paths
// (or an error) and records whether it was invoked, so a test can assert the
// managed branch calls it and the host-npx branch never does. No network, no
// Docker, no filesystem.
type fakeProvisioner struct {
	managed ManagedToolchain
	err     error
	calls   int
}

func (f *fakeProvisioner) Provision(_ context.Context) (ManagedToolchain, error) {
	f.calls++
	if f.err != nil {
		return ManagedToolchain{}, f.err
	}
	return f.managed, nil
}

// featuresPlan is the minimal Tier 1 plan that forces the Features (CLI) build
// branch.
func featuresPlan(buildArgs map[string]string) composition.Plan {
	return composition.Plan{
		Tier:      composition.TierDevcontainer,
		Features:  map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		BuildArgs: buildArgs,
	}
}

// managedArgvTail is the byte-identical argv tail both the host-npx and managed
// branches append after their invocation prefix. Centralized so the managed
// tests pin exactly the same tail the host-npx regression guard pins.
func managedArgvTail(dir, tag string, buildArgs ...string) []string {
	tail := []string{"build", "--workspace-folder", dir, "--image-name", tag}
	return append(tail, buildArgs...)
}

func TestBuildManagedToolchainUsesProvisionedPaths(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	prov := &fakeProvisioner{managed: ManagedToolchain{
		NodeBinary:    "/managed/node",
		CLIEntrypoint: "/managed/cli.js",
	}}
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner, Provisioner: prov}.Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeManaged,
		Plan:          featuresPlan(map[string]string{"Z": "last", "A": "first"}),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if prov.calls != 1 {
		t.Fatalf("provisioner calls = %d, want 1", prov.calls)
	}
	wantTag := ProjectImageTag(dir)
	if !result.Built || result.Image != wantTag {
		t.Fatalf("result = %+v, want built tag %s", result, wantTag)
	}
	if runner.name != "/managed/node" {
		t.Fatalf("runner.name = %q, want managed node path", runner.name)
	}
	want := append([]string{"/managed/cli.js"}, managedArgvTail(dir, wantTag, "--build-arg", "A=first", "--build-arg", "Z=last")...)
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBuildManagedArgvTailMatchesHostNPX(t *testing.T) {
	// The argv tail (everything after the invocation prefix) must be
	// byte-identical between the host-npx and managed branches. Build both for
	// the same plan and compare the post-prefix tail.
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	plan := featuresPlan(map[string]string{"A": "first"})

	hostRunner := &recordingRunner{}
	if _, err := (Builder{Runtime: "docker", Runner: hostRunner}).Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		Plan:          plan,
	}); err != nil {
		t.Fatalf("host-npx Build: %v", err)
	}

	managedRunner := &recordingRunner{}
	prov := &fakeProvisioner{managed: ManagedToolchain{NodeBinary: "/n/node", CLIEntrypoint: "/n/cli.js"}}
	if _, err := (Builder{Runtime: "docker", Runner: managedRunner, Provisioner: prov}).Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeManaged,
		Plan:          plan,
	}); err != nil {
		t.Fatalf("managed Build: %v", err)
	}

	// Host-npx prefix is len(devcontainerCLI)-1 args (name + tail). Managed
	// prefix is the single CLI entrypoint arg before the tail.
	hostTail := hostRunner.args[len(devcontainerCLI)-1:]
	managedTail := managedRunner.args[1:]
	if !reflect.DeepEqual(hostTail, managedTail) {
		t.Fatalf("argv tails differ:\n host=%#v\n managed=%#v", hostTail, managedTail)
	}
	wantTail := managedArgvTail(dir, ProjectImageTag(dir), "--build-arg", "A=first")
	if !reflect.DeepEqual(managedTail, wantTail) {
		t.Fatalf("managed tail = %#v, want %#v", managedTail, wantTail)
	}
}

func TestBuildDefaultUsesManagedToolchain(t *testing.T) {
	// The default selection (no ToolchainMode) must take the managed branch,
	// invoke the provisioner once, and run the provisioned managed prefix. This
	// is the #1530 flip: managed is now the default, host-npx the opt-out.
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	prov := &fakeProvisioner{managed: ManagedToolchain{NodeBinary: "/managed/node", CLIEntrypoint: "/managed/cli.js"}}
	runner := &recordingRunner{}
	if _, err := (Builder{Runtime: "docker", Runner: runner, Provisioner: prov}).Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Plan:    featuresPlan(nil),
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if prov.calls != 1 {
		t.Fatalf("provisioner called %d times on the managed default; want 1", prov.calls)
	}
	if runner.name != "/managed/node" {
		t.Fatalf("runner.name = %q, want managed node path %q", runner.name, "/managed/node")
	}
}

func TestBuildHostNPXExplicitModeNeverProvisions(t *testing.T) {
	// The explicit host-npx opt-out takes the host-npx branch and never touches
	// the provisioner, preserving the Docker-only/no-network unit path.
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	prov := &fakeProvisioner{managed: ManagedToolchain{NodeBinary: "/n", CLIEntrypoint: "/c"}}
	runner := &recordingRunner{}
	if _, err := (Builder{Runtime: "docker", Runner: runner, Provisioner: prov}).Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		Plan:          featuresPlan(nil),
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if prov.calls != 0 {
		t.Fatalf("provisioner called %d times for explicit host-npx; want 0", prov.calls)
	}
	if runner.name != devcontainerCLI[0] {
		t.Fatalf("runner.name = %q, want host-npx %q", runner.name, devcontainerCLI[0])
	}
}

func TestBuildManagedEscapeHatchUsesSuppliedPaths(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	node := filepath.Join(dir, "node")
	cli := filepath.Join(dir, "cli.js")
	for _, p := range []string{node, cli} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// A provisioner is present but must be bypassed by the escape hatch.
	prov := &fakeProvisioner{managed: ManagedToolchain{NodeBinary: "/should/not/use", CLIEntrypoint: "/should/not/use"}}
	runner := &recordingRunner{}
	if _, err := (Builder{Runtime: "docker", Runner: runner, Provisioner: prov}).Build(context.Background(), BuildOptions{
		WorkDir:                   dir,
		ToolchainMode:             ToolchainModeManaged,
		NodeBinary:                node,
		DevcontainerCLIEntrypoint: cli,
		Plan:                      featuresPlan(nil),
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if prov.calls != 0 {
		t.Fatalf("escape hatch must bypass the provisioner; calls = %d", prov.calls)
	}
	if runner.name != node {
		t.Fatalf("runner.name = %q, want escape-hatch node %q", runner.name, node)
	}
	if len(runner.args) == 0 || runner.args[0] != cli {
		t.Fatalf("runner.args[0] = %q, want escape-hatch cli %q", runner.args, cli)
	}
}

func TestBuildManagedEscapeHatchMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	cli := filepath.Join(dir, "cli.js")
	if err := os.WriteFile(cli, []byte("x"), 0o644); err != nil {
		t.Fatalf("write cli: %v", err)
	}
	_, err := (Builder{Runtime: "docker", Runner: &recordingRunner{}}).Build(context.Background(), BuildOptions{
		WorkDir:                   dir,
		ToolchainMode:             ToolchainModeManaged,
		NodeBinary:                filepath.Join(dir, "missing-node"),
		DevcontainerCLIEntrypoint: cli,
		Plan:                      featuresPlan(nil),
	})
	if err == nil {
		t.Fatal("Build: want error for missing escape-hatch node binary, got nil")
	}
}

func TestBuildManagedEscapeHatchPartialErrors(t *testing.T) {
	// Only one half of the escape hatch supplied: must error rather than
	// provisioning the missing half.
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	node := filepath.Join(dir, "node")
	if err := os.WriteFile(node, []byte("x"), 0o755); err != nil {
		t.Fatalf("write node: %v", err)
	}
	prov := &fakeProvisioner{managed: ManagedToolchain{NodeBinary: "/n", CLIEntrypoint: "/c"}}
	_, err := (Builder{Runtime: "docker", Runner: &recordingRunner{}, Provisioner: prov}).Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeManaged,
		NodeBinary:    node, // CLI entrypoint deliberately omitted
		Plan:          featuresPlan(nil),
	})
	if err == nil {
		t.Fatal("Build: want error for half-configured escape hatch, got nil")
	}
	if prov.calls != 0 {
		t.Fatalf("half-configured escape hatch must not provision; calls = %d", prov.calls)
	}
}

func TestBuildManagedNoProvisionerNoEscapeHatchErrors(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	_, err := (Builder{Runtime: "docker", Runner: &recordingRunner{}}).Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeManaged,
		Plan:          featuresPlan(nil),
	})
	if err == nil {
		t.Fatal("Build: want error when managed selected with no provisioner and no escape hatch, got nil")
	}
}

func TestBuildManagedProvisionerErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	sentinel := errors.New("fetch boom")
	prov := &fakeProvisioner{err: sentinel}
	_, err := (Builder{Runtime: "docker", Runner: &recordingRunner{}, Provisioner: prov}).Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeManaged,
		Plan:          featuresPlan(nil),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Build err = %v, want wrap of %v", err, sentinel)
	}
	// A genuine managed-provision failure must lead the operator to the manual
	// fallback: a host-provided Node via the escape hatch, or the host-npx opt-out.
	for _, want := range []string{"AILERON_SANDBOX_NODE", "--toolchain=host-npx"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("provision-failure error %q missing manual-fallback hint %q", err.Error(), want)
		}
	}
}

func TestResolveToolchainSelection(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	t.Run("default is managed with no escape hatch", func(t *testing.T) {
		mode, node, cli := ResolveToolchainSelection("", "", "", env(nil))
		if mode != ToolchainModeManaged || node != "" || cli != "" {
			t.Fatalf("got mode=%q node=%q cli=%q", mode, node, cli)
		}
	})
	t.Run("env selects host-npx opt-out", func(t *testing.T) {
		mode, _, _ := ResolveToolchainSelection("", "", "", env(map[string]string{ToolchainModeEnv: "host-npx"}))
		if mode != ToolchainModeHostNPX {
			t.Fatalf("mode = %q, want host-npx", mode)
		}
	})
	t.Run("flag overrides env", func(t *testing.T) {
		mode, _, _ := ResolveToolchainSelection("managed", "", "", env(map[string]string{ToolchainModeEnv: "host-npx"}))
		if mode != ToolchainModeManaged {
			t.Fatalf("mode = %q, want managed (flag override)", mode)
		}
	})
	t.Run("escape hatch from env", func(t *testing.T) {
		_, node, cli := ResolveToolchainSelection("", "", "", env(map[string]string{
			NodeBinaryEnv:      "/n",
			DevcontainerCLIEnv: "/c",
		}))
		if node != "/n" || cli != "/c" {
			t.Fatalf("node=%q cli=%q, want /n,/c", node, cli)
		}
	})
	t.Run("escape hatch flag overrides env", func(t *testing.T) {
		_, node, cli := ResolveToolchainSelection("", "/flagnode", "/flagcli", env(map[string]string{
			NodeBinaryEnv:      "/n",
			DevcontainerCLIEnv: "/c",
		}))
		if node != "/flagnode" || cli != "/flagcli" {
			t.Fatalf("node=%q cli=%q, want flag values", node, cli)
		}
	})
	t.Run("nil getenv is safe", func(t *testing.T) {
		mode, _, _ := ResolveToolchainSelection("", "", "", nil)
		if mode != ToolchainModeManaged {
			t.Fatalf("mode = %q, want managed", mode)
		}
	})
}

func TestIsManagedToolchain(t *testing.T) {
	// The default ("") and any unrecognized value now report managed; only the
	// explicit host-npx opt-out reports not-managed.
	if IsManagedToolchain("host-npx") {
		t.Fatal("explicit host-npx opt-out reported managed")
	}
	if !IsManagedToolchain("") || !IsManagedToolchain("bogus") {
		t.Fatal("default/unknown modes not reported managed")
	}
	if !IsManagedToolchain("managed") || !IsManagedToolchain("MANAGED") {
		t.Fatal("managed mode not reported managed")
	}
}

func TestResolveToolchainMode(t *testing.T) {
	cases := map[string]string{
		"":           ToolchainModeManaged,
		"host-npx":   ToolchainModeHostNPX,
		"HOST-NPX":   ToolchainModeHostNPX,
		"managed":    ToolchainModeManaged,
		"MANAGED":    ToolchainModeManaged,
		"  managed ": ToolchainModeManaged,
		"bogus":      ToolchainModeManaged,
	}
	for in, want := range cases {
		if got := resolveToolchainMode(in); got != want {
			t.Fatalf("resolveToolchainMode(%q) = %q, want %q", in, got, want)
		}
	}
}
