package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/composition"
)

type recordingRunner struct {
	name string
	args []string
}

func (r *recordingRunner) Run(_ context.Context, name string, args []string, _, _ io.Writer) error {
	r.name = name
	r.args = append([]string(nil), args...)
	return nil
}

type callRecordingRunner struct {
	calls []runnerCall
	errs  []error
}

type runnerCall struct {
	name string
	args []string
}

func (r *callRecordingRunner) Run(_ context.Context, name string, args []string, _, _ io.Writer) error {
	r.calls = append(r.calls, runnerCall{name: name, args: append([]string(nil), args...)})
	if len(r.errs) == 0 {
		return nil
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	return err
}

func TestBuildBaseImageUsesLocalContainerfile(t *testing.T) {
	dir := t.TempDir()
	containerfile := filepath.Join(dir, "images", "sandbox-base", "Containerfile")
	if err := os.MkdirAll(filepath.Dir(containerfile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(containerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Containerfile: %v", err)
	}
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !result.Built || result.Image != "aileron/sandbox-base:test" {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"build", "-t", "aileron/sandbox-base:test", "-f", containerfile, filepath.Dir(containerfile)}
	if runner.name != "docker" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("runner = %s %#v, want docker %#v", runner.name, runner.args, want)
	}
}

func TestBuildDevcontainerUsesProjectTagAndBuildArgs(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, ".devcontainer", "Dockerfile")
	if err := os.MkdirAll(filepath.Dir(dockerfile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Plan: composition.Plan{
			Tier:           composition.TierDevcontainer,
			DockerfilePath: "Dockerfile",
			BuildArgs:      map[string]string{"Z": "last", "A": "first"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantTag := ProjectImageTag(dir)
	if !result.Built || result.Image != wantTag {
		t.Fatalf("result = %+v, want built tag %s", result, wantTag)
	}
	want := []string{"build", "-t", wantTag, "-f", dockerfile, "--build-arg", "A=first", "--build-arg", "Z=last", dir}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBuildBaseImageAutoSkipsExistingImage(t *testing.T) {
	dir := t.TempDir()
	containerfile := filepath.Join(dir, "images", "sandbox-base", "Containerfile")
	if err := os.MkdirAll(filepath.Dir(containerfile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(containerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Containerfile: %v", err)
	}
	runner := &callRecordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Policy:  BuildPolicyAuto,
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Built {
		t.Fatalf("result.Built = true, want false")
	}
	want := []runnerCall{{name: "docker", args: []string{"image", "inspect", "aileron/sandbox-base:test"}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestBuildBaseImageAutoBuildsMissingImage(t *testing.T) {
	dir := t.TempDir()
	containerfile := filepath.Join(dir, "images", "sandbox-base", "Containerfile")
	if err := os.MkdirAll(filepath.Dir(containerfile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(containerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Containerfile: %v", err)
	}
	runner := &callRecordingRunner{errs: []error{errors.New("missing"), nil}}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Policy:  BuildPolicyAuto,
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !result.Built {
		t.Fatalf("result.Built = false, want true")
	}
	want := []runnerCall{
		{name: "docker", args: []string{"image", "inspect", "aileron/sandbox-base:test"}},
		{name: "docker", args: []string{"build", "-t", "aileron/sandbox-base:test", "-f", containerfile, filepath.Dir(containerfile)}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestBuildBaseImageNeverRequiresExistingImage(t *testing.T) {
	runner := &callRecordingRunner{errs: []error{errors.New("missing")}}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyNever,
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err == nil {
		t.Fatal("expected missing image error")
	}
	if !strings.Contains(err.Error(), "sandbox build policy is never") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildBaseImageNeverSkipsExistingImage(t *testing.T) {
	runner := &callRecordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyNever,
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Built {
		t.Fatalf("result.Built = true, want false")
	}
	want := []runnerCall{{name: "docker", args: []string{"image", "inspect", "aileron/sandbox-base:test"}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestBuildRejectsUnsupportedPolicy(t *testing.T) {
	_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  "sometimes",
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err == nil {
		t.Fatal("expected unsupported policy error")
	}
}

func TestShouldBuildRejectsUnsupportedPolicy(t *testing.T) {
	_, err := (Builder{}).shouldBuild(context.Background(), &recordingRunner{}, "docker", "image:test", "sometimes")
	if err == nil {
		t.Fatal("expected unsupported policy error")
	}
}

func TestBuildBYOImageDoesNotBuild(t *testing.T) {
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "definitely-not-a-runtime", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Plan: composition.Plan{
			Tier:  composition.TierBYOImage,
			Image: "ghcr.io/acme/agent:latest",
		},
	})
	if err != ErrNoBuildRequired {
		t.Fatalf("err = %v, want ErrNoBuildRequired", err)
	}
	if result.Built || result.Image != "ghcr.io/acme/agent:latest" {
		t.Fatalf("result = %+v", result)
	}
	if runner.args != nil {
		t.Fatalf("runner unexpectedly called: %#v", runner.args)
	}
}

func TestBuildDevcontainerImageDoesNotRequireRuntime(t *testing.T) {
	result, err := Builder{Runtime: "definitely-not-a-runtime"}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Plan: composition.Plan{
			Tier:  composition.TierDevcontainer,
			Image: "mcr.microsoft.com/devcontainers/base:ubuntu",
		},
	})
	if err != ErrNoBuildRequired {
		t.Fatalf("err = %v, want ErrNoBuildRequired", err)
	}
	if result.Image != "mcr.microsoft.com/devcontainers/base:ubuntu" {
		t.Fatalf("result = %+v", result)
	}
}

func TestBuildBaseImageRejectsUnsupportedRuntime(t *testing.T) {
	_, err := Builder{Runtime: "definitely-not-a-runtime"}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err == nil {
		t.Fatal("expected unsupported runtime error")
	}
}

func TestBuildDevcontainerMissingDockerfile(t *testing.T) {
	_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Plan: composition.Plan{
			Tier:           composition.TierDevcontainer,
			DockerfilePath: "Dockerfile",
		},
	})
	if err == nil {
		t.Fatal("expected missing Dockerfile error")
	}
}

func TestResolveRuntimeRejectsUnsupportedRuntime(t *testing.T) {
	_, err := ResolveRuntime("nerdctl")
	if err == nil {
		t.Fatal("expected unsupported runtime error")
	}
}

func TestResolveRuntimeReportsMissingExplicitRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ResolveRuntime("docker")
	if err == nil {
		t.Fatal("expected missing docker error")
	}
}

func TestResolveRuntimeAutoFindsDocker(t *testing.T) {
	dir := t.TempDir()
	name := "docker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte{}, 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	got, err := ResolveRuntime(DefaultRuntime)
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if got != "docker" {
		t.Fatalf("runtime = %q, want docker", got)
	}
}

func TestResolveRuntimeAutoReportsMissingRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ResolveRuntime(DefaultRuntime)
	if err == nil {
		t.Fatal("expected missing runtime error")
	}
}

func TestResolveRuntimeWithInjectedRunnerSkipsPathCheck(t *testing.T) {
	got, err := resolveRuntime("docker", false)
	if err != nil {
		t.Fatalf("resolveRuntime: %v", err)
	}
	if got != "docker" {
		t.Fatalf("runtime = %q, want docker", got)
	}
}

func TestExecRunnerRun(t *testing.T) {
	if err := (execRunner{}).Run(context.Background(), "go", []string{"version"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("execRunner.Run: %v", err)
	}
}

func TestBaseBuildArgsReportsMissingContext(t *testing.T) {
	_, err := baseBuildArgs(t.TempDir(), "aileron/sandbox-base:test")
	if err == nil {
		t.Fatal("expected missing context error")
	}
}

func TestFindBaseContextHonorsEnvOverride(t *testing.T) {
	t.Setenv("AILERON_SANDBOX_BASE_CONTEXT", "/custom/context")
	got, err := findBaseContext(t.TempDir())
	if err != nil {
		t.Fatalf("findBaseContext: %v", err)
	}
	if got != "/custom/context" {
		t.Fatalf("context = %q", got)
	}
}

func TestFindBaseContextWalksParents(t *testing.T) {
	dir := t.TempDir()
	contextDir := filepath.Join(dir, "images", "sandbox-base")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Containerfile: %v", err)
	}
	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	got, err := findBaseContext(nested)
	if err != nil {
		t.Fatalf("findBaseContext: %v", err)
	}
	if got != contextDir {
		t.Fatalf("context = %q, want %q", got, contextDir)
	}
}

func TestFindBaseContextReportsMissingContext(t *testing.T) {
	_, err := findBaseContext(t.TempDir())
	if err == nil {
		t.Fatal("expected missing context error")
	}
}

func TestResolveDockerfileAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	got, err := resolveDockerfile(t.TempDir(), composition.Plan{DockerfilePath: dockerfile})
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if got != dockerfile {
		t.Fatalf("dockerfile = %q, want %q", got, dockerfile)
	}
}

func TestBuilderStreamsRuntimeOutput(t *testing.T) {
	dir := t.TempDir()
	containerfile := filepath.Join(dir, "images", "sandbox-base", "Containerfile")
	if err := os.MkdirAll(filepath.Dir(containerfile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(containerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Containerfile: %v", err)
	}
	var stdout bytes.Buffer
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, out, _ io.Writer) error {
		_, _ = out.Write([]byte("building\n"))
		return nil
	})
	_, err := Builder{Runtime: "docker", Runner: runner, Stdout: &stdout}.Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Plan: composition.Plan{
			Tier:  composition.TierBase,
			Image: "aileron/sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stdout.String() != "building\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

// pinHostOSDarwin overrides the package-level hostOS indirection so a
// test's argv expectations stay platform-independent. The Linux Docker
// branch (--add-host=host.docker.internal:host-gateway) has its own
// targeted tests; here we only care about the generic argv shape.
func pinHostOSDarwin(t *testing.T) {
	t.Helper()
	orig := hostOS
	hostOS = func() string { return "darwin" }
	t.Cleanup(func() { hostOS = orig })
}

func TestRunMountsWorkspaceAndExecutesCommand(t *testing.T) {
	pinHostOSDarwin(t)
	dir := t.TempDir()
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:   "aileron/sandbox-base:test",
		WorkDir: dir,
		Env: map[string]string{
			"Z_VAR": "last",
			"A_VAR": "first",
		},
		Command: []string{"codex", "--model", "gpt-5"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Runtime != "docker" {
		t.Fatalf("runtime = %q, want docker", result.Runtime)
	}
	want := []string{
		"run", "--rm", "-i",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"--env", "A_VAR=first",
		"--env", "Z_VAR=last",
		"aileron/sandbox-base:test",
		"codex", "--model", "gpt-5",
	}
	if runner.name != "docker" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("runner = %s %#v, want docker %#v", runner.name, runner.args, want)
	}
}

func TestRunCanOverrideContainerUser(t *testing.T) {
	pinHostOSDarwin(t)
	dir := t.TempDir()
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:   "aileron/sandbox-base:test",
		WorkDir: dir,
		User:    "root",
		Command: []string{"aileron-run-with-proxy-ca", "codex"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"run", "--rm", "-i",
		"--user", "root",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"aileron/sandbox-base:test",
		"aileron-run-with-proxy-ca", "codex",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestRunMountsAdditionalReadOnlyVolumes(t *testing.T) {
	pinHostOSDarwin(t)
	dir := t.TempDir()
	extra := filepath.Join(dir, "actions")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:   "aileron/sandbox-base:test",
		WorkDir: dir,
		Volumes: []Volume{{
			Source:   extra,
			Target:   "/opt/aileron/manifests/actions",
			ReadOnly: true,
		}},
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	absExtra, _ := filepath.Abs(extra)
	want := []string{
		"run", "--rm", "-i",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"--volume", absExtra + ":/opt/aileron/manifests/actions:ro",
		"aileron/sandbox-base:test",
		"codex",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestRunMountsAdditionalReadWriteVolumes(t *testing.T) {
	dir := t.TempDir()
	extra := filepath.Join(dir, "connectors")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:   "aileron/sandbox-base:test",
		WorkDir: dir,
		Volumes: []Volume{{
			Source: extra,
			Target: "/opt/aileron/manifests/connectors",
		}},
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	absExtra, _ := filepath.Abs(extra)
	want := "--volume"
	found := false
	for i := 0; i < len(runner.args)-1; i++ {
		if runner.args[i] == want && runner.args[i+1] == absExtra+":/opt/aileron/manifests/connectors" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("read-write volume missing from args: %#v", runner.args)
	}
}

func TestRunRejectsIncompleteAdditionalVolume(t *testing.T) {
	_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Run(context.Background(), RunOptions{
		Image:   "aileron/sandbox-base:test",
		Volumes: []Volume{{Target: "/opt/aileron/manifests/actions"}},
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected incomplete volume error")
	}
}

func TestRunAddsTTYWhenRequested(t *testing.T) {
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "podman", Runner: runner}.Run(context.Background(), RunOptions{
		Image:   "aileron/sandbox-base:test",
		Command: []string{"claude"},
		TTY:     true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(runner.args[:4], []string{"run", "--rm", "-i", "-t"}) {
		t.Fatalf("args prefix = %#v, want tty run prefix", runner.args[:4])
	}
}

func TestRunRejectsMissingImage(t *testing.T) {
	_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Run(context.Background(), RunOptions{
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected missing image error")
	}
}

func TestRunRejectsMissingCommand(t *testing.T) {
	_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Run(context.Background(), RunOptions{
		Image: "aileron/sandbox-base:test",
	})
	if err == nil {
		t.Fatal("expected missing command error")
	}
}

func TestValidateRunsMinimalContractProbe(t *testing.T) {
	pinHostOSDarwin(t)
	dir := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "tools.txt")
	if err := os.WriteFile(manifest, []byte("tool\tfqn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: dir,
		Env: map[string]string{
			"HTTPS_PROXY": "http://host.docker.internal:48123",
		},
		Volumes: []Volume{{
			Source:   manifest,
			Target:   "/etc/aileron/tools.txt",
			ReadOnly: true,
		}},
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if runner.name != "docker" {
		t.Fatalf("runtime = %q, want docker", runner.name)
	}
	wantPrefix := []string{
		"run", "--rm", "-i",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"--volume", manifest + ":/etc/aileron/tools.txt:ro",
		"--env", "HTTPS_PROXY=http://host.docker.internal:48123",
		"ghcr.io/acme/agent:latest",
		"/bin/sh", "-c",
	}
	if len(runner.args) < len(wantPrefix)+4 {
		t.Fatalf("args too short: %#v", runner.args)
	}
	if !reflect.DeepEqual(runner.args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("args prefix = %#v, want %#v", runner.args[:len(wantPrefix)], wantPrefix)
	}
	if runner.args[len(runner.args)-4] != "codex" {
		t.Fatalf("validation command = %q, want codex", runner.args[len(runner.args)-4])
	}
	if runner.args[len(runner.args)-3] != "0" {
		t.Fatalf("shim validation flag = %q, want 0", runner.args[len(runner.args)-3])
	}
	if runner.args[len(runner.args)-2] != "0" {
		t.Fatalf("proxy trust validation flag = %q, want 0", runner.args[len(runner.args)-2])
	}
	if runner.args[len(runner.args)-1] != "0" {
		t.Fatalf("mcp binary validation flag = %q, want 0", runner.args[len(runner.args)-1])
	}
}

func TestValidateRequiresWgetWhenShimsAreMounted(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(t.TempDir(), "google")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: dir,
		Volumes: []Volume{{
			Source:   shim,
			Target:   "/usr/local/bin/google",
			ReadOnly: true,
		}},
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if runner.args[len(runner.args)-3] != "1" {
		t.Fatalf("shim validation flag = %q, want 1", runner.args[len(runner.args)-3])
	}
	if runner.args[len(runner.args)-2] != "0" {
		t.Fatalf("proxy trust validation flag = %q, want 0", runner.args[len(runner.args)-2])
	}
	if runner.args[len(runner.args)-1] != "0" {
		t.Fatalf("mcp binary validation flag = %q, want 0", runner.args[len(runner.args)-1])
	}
	script := runner.args[len(runner.args)-6]
	if !strings.Contains(script, "generated Aileron connector shims require wget") {
		t.Fatalf("validation script did not include wget requirement:\n%s", script)
	}
	if !strings.Contains(script, `"--post-data"`) {
		t.Fatalf("validation script did not include wget flag probe:\n%s", script)
	}
}

func TestValidateRequiresProxyTrustHelperWhenRequested(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: dir,
		Env: map[string]string{
			"AILERON_SANDBOX_PROXY_CA_FILE": "/etc/aileron/proxy/ca.pem",
			"AILERON_SANDBOX_PROXY_MODE":    "bootstrap",
		},
		Volumes: []Volume{{
			Source:   ca,
			Target:   "/etc/aileron/proxy/ca.pem",
			ReadOnly: true,
		}},
		Command:           []string{"codex"},
		RequireProxyTrust: true,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if runner.args[len(runner.args)-4] != "codex" {
		t.Fatalf("validation command = %q, want codex", runner.args[len(runner.args)-4])
	}
	if runner.args[len(runner.args)-3] != "0" {
		t.Fatalf("shim validation flag = %q, want 0", runner.args[len(runner.args)-3])
	}
	if runner.args[len(runner.args)-2] != "1" {
		t.Fatalf("proxy trust validation flag = %q, want 1", runner.args[len(runner.args)-2])
	}
	if runner.args[len(runner.args)-1] != "0" {
		t.Fatalf("mcp binary validation flag = %q, want 0", runner.args[len(runner.args)-1])
	}
	script := runner.args[len(runner.args)-6]
	if !strings.Contains(script, "aileron-install-proxy-ca --check") {
		t.Fatalf("validation script missing proxy trust helper check:\n%s", script)
	}
	if !strings.Contains(script, "command -v aileron-run-with-proxy-ca") {
		t.Fatalf("validation script missing proxy wrapper check:\n%s", script)
	}
}

func TestValidateReportsActionableRuntimeFailure(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
		_, _ = stderr.Write([]byte("agent command not found in sandbox image: codex\n"))
		return errors.New("exit status 127")
	})
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: t.TempDir(),
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"validate sandbox image ghcr.io/acme/agent:latest",
		"agent command not found in sandbox image: codex",
		AgentImagesDocsURL,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}

func TestValidateReportsMissingWgetForShimImages(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
		_, _ = stderr.Write([]byte("generated Aileron connector shims require wget in the sandbox image\n"))
		return errors.New("exit status 127")
	})
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: t.TempDir(),
		Volumes: []Volume{{
			Source: filepath.Join(t.TempDir(), "google"),
			Target: "/usr/local/bin/google",
		}},
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"validate sandbox image ghcr.io/acme/agent:latest",
		"generated Aileron connector shims require wget in the sandbox image",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}

func TestValidateReportsUnsupportedWgetForShimImages(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
		_, _ = stderr.Write([]byte("generated Aileron connector shims require wget support for --post-data\n"))
		return errors.New("exit status 127")
	})
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: t.TempDir(),
		Volumes: []Volume{{
			Source: filepath.Join(t.TempDir(), "google"),
			Target: "/usr/local/bin/google",
		}},
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"validate sandbox image ghcr.io/acme/agent:latest",
		"generated Aileron connector shims require wget support for --post-data",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}

func TestValidateRejectsMissingImage(t *testing.T) {
	err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Validate(context.Background(), ValidateOptions{
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected missing image error")
	}
}

func TestValidateRejectsMissingCommand(t *testing.T) {
	err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Validate(context.Background(), ValidateOptions{
		Image: "ghcr.io/acme/agent:latest",
	})
	if err == nil {
		t.Fatal("expected missing command error")
	}
}

func TestValidateReportsFallbackWhenRuntimeHasNoDetail(t *testing.T) {
	runner := runnerFunc(func(context.Context, string, []string, io.Writer, io.Writer) error {
		return errors.New("exit status 126")
	})
	err := Builder{
		Runtime: "docker",
		Runner:  runner,
		Stderr:  io.Discard,
	}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: t.TempDir(),
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"image must support /bin/sh command execution",
		"agent command \"codex\" on PATH",
		AgentImagesDocsURL,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}

type runnerFunc func(context.Context, string, []string, io.Writer, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return f(ctx, name, args, stdout, stderr)
}

// --- Validate-script aileron-mcp presence (U4 / #953) ---

func TestValidate_RequireMCPBinary_AppendsFourthPositional(t *testing.T) {
	dir := t.TempDir()
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:            "ghcr.io/acme/agent:latest",
		WorkDir:          dir,
		Command:          []string{"codex"},
		RequireMCPBinary: true,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if runner.args[len(runner.args)-1] != "1" {
		t.Fatalf("mcp binary validation flag = %q, want 1", runner.args[len(runner.args)-1])
	}
	script := runner.args[len(runner.args)-6]
	for _, want := range []string{
		`if [ "${4:-0}" = "1" ]; then`,
		"command -v aileron-mcp",
		"aileron-mcp --version",
		"sandbox MCP wiring failed",
		"arch mismatch",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("validation script missing %q:\n%s", want, script)
		}
	}
}

func TestValidate_RequireMCPBinary_OmittedByDefault(t *testing.T) {
	dir := t.TempDir()
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: dir,
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if runner.args[len(runner.args)-1] != "0" {
		t.Fatalf("mcp binary validation flag = %q, want 0 (default)", runner.args[len(runner.args)-1])
	}
}

// --- runArgs --add-host=host.docker.internal:host-gateway (U2 / #953) ---

// runOptsForGateway returns the minimal RunOptions that lets runArgs
// compose a valid argv. Used by the host-gateway-flag tests below.
func runOptsForGateway(t *testing.T) RunOptions {
	t.Helper()
	dir := t.TempDir()
	return RunOptions{
		Image:   "img",
		WorkDir: dir,
		Command: []string{"sh"},
	}
}

func TestRunArgs_IncludesNameWhenSet(t *testing.T) {
	opts := runOptsForGateway(t)
	opts.Name = "aileron-sbx-x"
	args, err := runArgs("docker", opts)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "--name aileron-sbx-x") {
		t.Errorf("expected --name aileron-sbx-x in args; got %v", args)
	}
}

func TestRunArgs_OmitsNameWhenEmpty(t *testing.T) {
	args, err := runArgs("docker", runOptsForGateway(t))
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, a := range args {
		if a == "--name" {
			t.Errorf("did not expect --name when Name is empty; got %v", args)
		}
	}
}

func TestStopContainerIssuesStopWithGrace(t *testing.T) {
	runner := &recordingRunner{}
	if err := StopContainer(context.Background(), runner, "docker", "aileron-sbx-x", 10, io.Discard, io.Discard); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	want := []string{"stop", "--time", "10", "aileron-sbx-x"}
	if runner.name != "docker" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("runner = %s %#v, want docker %#v", runner.name, runner.args, want)
	}
}

func TestStopContainerReturnsRunnerErrorVerbatim(t *testing.T) {
	// A genuine error whose stderr does not match the already-gone
	// signature is returned verbatim — only the auto-removal race is
	// suppressed, not real failures.
	wantErr := errors.New("permission denied")
	runner := &callRecordingRunner{errs: []error{wantErr}}
	err := StopContainer(context.Background(), runner, "podman", "aileron-sbx-y", 10, io.Discard, io.Discard)
	if !errors.Is(err, wantErr) {
		t.Fatalf("StopContainer error = %v, want %v", err, wantErr)
	}
}

// stderrWritingRunner writes a fixed string to the stderr writer and
// then returns errReturn, modeling a container runtime that prints a
// diagnostic to stderr and exits non-zero.
type stderrWritingRunner struct {
	stderrText string
	errReturn  error
}

func (r *stderrWritingRunner) Run(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
	_, _ = io.WriteString(stderr, r.stderrText)
	return r.errReturn
}

func TestStopContainerSuppressesAlreadyGone(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
	}{
		{"docker no such container", "Error response from daemon: No such container: aileron-sbx-x\n"},
		{"is not running", "Error: container aileron-sbx-x is not running\n"},
		{"already stopped", "Error: container already stopped\n"},
		{"mixed case", "ERROR RESPONSE FROM DAEMON: NO SUCH CONTAINER\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var userStderr bytes.Buffer
			runner := &stderrWritingRunner{stderrText: tc.stderr, errReturn: errors.New("exit status 1")}
			if err := StopContainer(context.Background(), runner, "docker", "aileron-sbx-x", 10, io.Discard, &userStderr); err != nil {
				t.Fatalf("StopContainer = %v, want nil (already-gone suppressed)", err)
			}
			// The runtime's stderr still reaches the caller's writer so
			// the user sees the runtime output; only the returned error
			// is suppressed.
			if !strings.Contains(userStderr.String(), strings.TrimSpace(tc.stderr)[:5]) {
				t.Fatalf("expected runtime stderr teed to caller; got %q", userStderr.String())
			}
		})
	}
}

func TestStopContainerGenuineStderrErrorReturned(t *testing.T) {
	wantErr := errors.New("exit status 1")
	runner := &stderrWritingRunner{
		stderrText: "Error response from daemon: permission denied while trying to connect to the Docker daemon socket\n",
		errReturn:  wantErr,
	}
	err := StopContainer(context.Background(), runner, "docker", "aileron-sbx-x", 10, io.Discard, io.Discard)
	if !errors.Is(err, wantErr) {
		t.Fatalf("StopContainer error = %v, want %v (genuine error not suppressed)", err, wantErr)
	}
}

func TestStopContainerEmptyNameIsNoOp(t *testing.T) {
	runner := &callRecordingRunner{errs: []error{errors.New("must not run")}}
	if err := StopContainer(context.Background(), runner, "docker", "", 10, io.Discard, io.Discard); err != nil {
		t.Fatalf("StopContainer empty name = %v, want nil", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected no runner calls for empty name; got %v", runner.calls)
	}
}

func TestRunArgs_LinuxDockerAddsHostGateway(t *testing.T) {
	orig := hostOS
	hostOS = func() string { return "linux" }
	defer func() { hostOS = orig }()

	args, err := runArgs("docker", runOptsForGateway(t))
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--add-host host.docker.internal:host-gateway") {
		t.Errorf("expected --add-host=host.docker.internal:host-gateway on Linux Docker; got %v", args)
	}
}

func TestRunArgs_MacOSDockerOmitsHostGateway(t *testing.T) {
	orig := hostOS
	hostOS = func() string { return "darwin" }
	defer func() { hostOS = orig }()

	args, err := runArgs("docker", runOptsForGateway(t))
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, a := range args {
		if a == "host.docker.internal:host-gateway" {
			t.Errorf("did not expect --add-host on macOS Docker; got %v", args)
		}
	}
}

func TestRunArgs_LinuxPodmanOmitsHostGateway(t *testing.T) {
	orig := hostOS
	hostOS = func() string { return "linux" }
	defer func() { hostOS = orig }()

	args, err := runArgs("podman", runOptsForGateway(t))
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, a := range args {
		if a == "host.docker.internal:host-gateway" {
			t.Errorf("did not expect --add-host on Linux Podman; got %v", args)
		}
	}
}
