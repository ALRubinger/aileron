package container

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestRunMountsWorkspaceAndExecutesCommand(t *testing.T) {
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

type runnerFunc func(context.Context, string, []string, io.Writer, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return f(ctx, name, args, stdout, stderr)
}
