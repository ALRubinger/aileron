package container

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestInteractiveTTYRun(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"run with tty", []string{"run", "--rm", "-i", "-t", "img", "claude"}, true},
		{"run without tty", []string{"run", "--rm", "-i", "img", "claude"}, false},
		{"exec with tty", []string{"exec", "-i", "-t", "c", "gh", "auth", "login"}, true},
		{"exec without tty", []string{"exec", "-i", "c", "gh", "auth", "token"}, false},
		{"build with tag flag is not a tty run", []string{"build", "-t", "img", "-f", "Dockerfile", "."}, false},
		{"image inspect", []string{"image", "inspect", "img"}, false},
		{"empty args", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := interactiveTTYRun(tc.args); got != tc.want {
				t.Fatalf("interactiveTTYRun(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
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
			Image: "aileron-sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !result.Built || result.Image != "aileron-sandbox-base:test" {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"build", "-t", "aileron-sandbox-base:test", "-f", containerfile, filepath.Dir(containerfile)}
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

// writeFeaturesDevcontainer writes a minimal .devcontainer/devcontainer.json so
// the features build path's existence check passes; the content is irrelevant
// to the unit-layer argv assertions (the real CLI reads it, the recordingRunner
// does not).
func writeFeaturesDevcontainer(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, composition.DefaultDevcontainerPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"features":{"./tool":{}}}`), 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}
}

func TestBuildDevcontainerWithFeaturesUsesDevcontainerCLI(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		Plan: composition.Plan{
			Tier:      composition.TierDevcontainer,
			Features:  map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
			BuildArgs: map[string]string{"Z": "last", "A": "first"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantTag := ProjectImageTag(dir)
	if !result.Built || result.Image != wantTag {
		t.Fatalf("result = %+v, want built tag %s", result, wantTag)
	}
	if runner.name != devcontainerCLI[0] {
		t.Fatalf("runner.name = %q, want %q", runner.name, devcontainerCLI[0])
	}
	want := append(append([]string(nil), devcontainerCLI[1:]...),
		"build", "--workspace-folder", dir, "--image-name", wantTag,
		"--build-arg", "A=first", "--build-arg", "Z=last")
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

// TestBuildDevcontainerWithFeaturesMultiArchEmitsOCILayout proves the Features
// build the freeze producer drives appends the buildx multi-platform + OCI-layout
// output tokens when BuildOptions.Platforms is set, and records the layout dir on
// the result. The composed freeze path exercises exactly this assembler.
func TestBuildDevcontainerWithFeaturesMultiArchEmitsOCILayout(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	dest := filepath.Join(t.TempDir(), "layout")
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		Platforms:     composition.MultiArchPlatforms,
		OCILayoutDest: dest,
		Plan: composition.Plan{
			Tier:     composition.TierDevcontainer,
			Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantTag := ProjectImageTag(dir)
	want := append(append([]string(nil), devcontainerCLI[1:]...),
		"build", "--workspace-folder", dir, "--image-name", wantTag,
		"--platform", "linux/amd64,linux/arm64",
		"--output", "type=oci,dest="+dest+",tar=false")
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
	if result.OCILayoutDir != dest {
		t.Fatalf("result.OCILayoutDir = %q, want %q", result.OCILayoutDir, dest)
	}
}

// envRecordingRunner records the env passed to each build invocation, so a test
// can assert a scoped BUILDX_BUILDER selection reaches the build subprocess. It
// implements the optional envRunner capability.
type envRecordingRunner struct {
	args []string
	env  []string
}

func (r *envRecordingRunner) Run(_ context.Context, _ string, args []string, _, _ io.Writer) error {
	r.args = append([]string(nil), args...)
	r.env = nil
	return nil
}

func (r *envRecordingRunner) RunWithEnv(_ context.Context, _ string, args, env []string, _, _ io.Writer) error {
	r.args = append([]string(nil), args...)
	r.env = append([]string(nil), env...)
	return nil
}

// TestBuildMultiArchScopesBuildxBuilder proves BuildOptions.BuildxBuilder scopes a
// multi-arch build to the named buildx builder via a BUILDX_BUILDER env entry on
// the build subprocess, without any `docker buildx use` (which would repoint the
// operator's default builder). Regression for #2054.
func TestBuildMultiArchScopesBuildxBuilder(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	dest := filepath.Join(t.TempDir(), "layout")
	runner := &envRecordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		Platforms:     composition.MultiArchPlatforms,
		OCILayoutDest: dest,
		BuildxBuilder: FreezeBuilderName,
		Plan: composition.Plan{
			Tier:     composition.TierDevcontainer,
			Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantEnv := "BUILDX_BUILDER=" + FreezeBuilderName
	found := false
	for _, e := range runner.env {
		if e == wantEnv {
			found = true
		}
	}
	if !found {
		t.Fatalf("build env = %#v, want it to carry %q", runner.env, wantEnv)
	}
	// The builder selection must be scoped to the env, never `buildx use`.
	for _, a := range runner.args {
		if a == "use" {
			t.Fatalf("multi-arch build must not `buildx use`, args = %#v", runner.args)
		}
	}
	// The determinate BUILDKIT_PROGRESS=rawjson env is gone (issue #2093): the
	// build subprocess env carries only the scoped builder selection.
	for _, e := range runner.env {
		if strings.HasPrefix(e, "BUILDKIT_PROGRESS=") {
			t.Errorf("build env = %#v, must not carry BUILDKIT_PROGRESS (determinate progress removed)", runner.env)
		}
	}
	if len(runner.env) != 1 {
		t.Errorf("multi-arch build env = %#v, want exactly the BUILDX_BUILDER entry", runner.env)
	}
}

// TestBuildSingleArchIgnoresBuildxBuilder proves BuildxBuilder is inert for a
// single-arch daemon-load build: no BUILDX_BUILDER env is exported, so that build
// stays on the default `docker` driver and lands the image in the local daemon
// (the docker-container freeze builder cannot). Regression for #2054.
func TestBuildSingleArchIgnoresBuildxBuilder(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	runner := &envRecordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		BuildxBuilder: FreezeBuilderName, // set but no Platforms -> must be ignored
		Plan: composition.Plan{
			Tier:     composition.TierDevcontainer,
			Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(runner.env) != 0 {
		t.Fatalf("single-arch build env = %#v, want no BUILDX_BUILDER (default driver must load into the daemon)", runner.env)
	}
}

// TestBuild_MultiArchPairingValidated proves Build fails fast on a half-configured
// multi-arch request rather than emitting a broken `--output type=oci,dest=` token.
func TestBuild_MultiArchPairingValidated(t *testing.T) {
	base := BuildOptions{
		Plan:   composition.Plan{Tier: composition.TierDevcontainer, Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")}},
		Policy: BuildPolicyAlways,
	}

	t.Run("platforms without dest", func(t *testing.T) {
		opts := base
		opts.Platforms = composition.MultiArchPlatforms
		_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Build(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "OCILayoutDest") {
			t.Fatalf("err = %v, want a missing-OCILayoutDest error", err)
		}
	})

	t.Run("dest without platforms", func(t *testing.T) {
		opts := base
		opts.OCILayoutDest = filepath.Join(t.TempDir(), "layout")
		_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Build(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "without any Platforms") {
			t.Fatalf("err = %v, want a dest-without-platforms error", err)
		}
	})
}

// TestDevcontainerCLIBuildArgs_SingleArchByteIdentical proves the assembler emits
// the exact same argv as before when Platforms is unset (no multi-arch tokens).
func TestDevcontainerCLIBuildArgs_SingleArchByteIdentical(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	prefix := []string{"npx", "devcontainer"}
	name, got, err := devcontainerCLIBuildArgs(prefix, dir, composition.Plan{Tier: composition.TierDevcontainer}, "img:tag", nil, "")
	if err != nil {
		t.Fatalf("devcontainerCLIBuildArgs: %v", err)
	}
	if name != "npx" {
		t.Fatalf("name = %q, want npx", name)
	}
	want := []string{"devcontainer", "build", "--workspace-folder", dir, "--image-name", "img:tag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("single-arch args = %#v, want byte-identical %#v", got, want)
	}
}

// TestBaseBuildArgs_MultiArchSwitchesToBuildx proves the raw base assembler
// switches the `build` verb to `buildx build` and appends the OCI-output tokens
// when platforms are requested, and stays plain `build` otherwise.
func TestBaseBuildArgs_MultiArchSwitchesToBuildx(t *testing.T) {
	t.Setenv("AILERON_SANDBOX_BASE_CONTEXT", t.TempDir())
	dest := filepath.Join(t.TempDir(), "layout")
	got, err := baseBuildArgs(t.TempDir(), "img:test", composition.MultiArchPlatforms, dest)
	if err != nil {
		t.Fatalf("baseBuildArgs: %v", err)
	}
	if len(got) < 2 || got[0] != "buildx" || got[1] != "build" {
		t.Fatalf("multi-arch base argv must lead with `buildx build`, got %#v", got)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--platform linux/amd64,linux/arm64") ||
		!strings.Contains(joined, "--output type=oci,dest="+dest+",tar=false") {
		t.Fatalf("multi-arch base argv missing buildx tokens: %#v", got)
	}
	single, err := baseBuildArgs(t.TempDir(), "img:test", nil, "")
	if err != nil {
		t.Fatalf("baseBuildArgs single: %v", err)
	}
	if single[0] != "build" {
		t.Fatalf("single-arch base argv must stay plain `build`, got %#v", single)
	}
}

// TestCheckMultiArchBuild covers the preflight outcomes over the Runner seam
// (Docker-free): buildx missing, the dedicated builder absent-then-provisioned,
// an arch missing after bootstrap, both arches present, and a bootstrap failure.
// On a platform/bootstrap miss the error must name the tonistiigi/binfmt remedy.
// The preflight targets the dedicated `aileron-freeze` builder, so its inspect and
// bootstrap keys carry that name.
func TestCheckMultiArchBuild(t *testing.T) {
	const binfmt = "tonistiigi/binfmt"
	const inspectFreeze = "buildx inspect " + FreezeBuilderName
	const bootstrapFreeze = "buildx inspect " + FreezeBuilderName + " --bootstrap"
	const createFreeze = "buildx create --name " + FreezeBuilderName + " --driver docker-container"

	t.Run("buildx missing", func(t *testing.T) {
		fr := &scriptedRunner{fails: map[string]error{"buildx version": errors.New("unknown command")}}
		err := CheckMultiArchBuild(context.Background(), fr, "docker")
		if err == nil || !strings.Contains(err.Error(), "buildx") || !strings.Contains(err.Error(), binfmt) {
			t.Fatalf("err = %v, want a buildx+binfmt remediation", err)
		}
	})

	t.Run("arch missing", func(t *testing.T) {
		// The existence probe succeeds (builder present) by default; the bootstrap
		// advertises only one required arch.
		fr := &scriptedRunner{outputs: map[string]string{
			bootstrapFreeze: "Platforms: linux/amd64, linux/386\n",
		}}
		err := CheckMultiArchBuild(context.Background(), fr, "docker")
		if err == nil || !strings.Contains(err.Error(), "linux/arm64") || !strings.Contains(err.Error(), binfmt) {
			t.Fatalf("err = %v, want a missing-arm64 remediation", err)
		}
	})

	t.Run("both arches present with builder already provisioned", func(t *testing.T) {
		fr := &scriptedRunner{outputs: map[string]string{
			bootstrapFreeze: "Driver: docker-container\nPlatforms: linux/amd64, linux/arm64, linux/arm/v7\n",
		}}
		if err := CheckMultiArchBuild(context.Background(), fr, "docker"); err != nil {
			t.Fatalf("both arches present must pass, got %v", err)
		}
		// The builder already existed (inspect probe succeeded), so it must not be
		// created again.
		if n := fr.countCalls(createFreeze); n != 0 {
			t.Fatalf("create called %d times, want 0 when the builder is already present", n)
		}
	})

	t.Run("default docker-driver setup auto-provisions the freeze builder", func(t *testing.T) {
		// Regression for #2054: on a default Docker Desktop setup the freeze builder
		// is absent (existence probe fails), so the preflight creates it exactly once
		// with the docker-container driver and does NOT hard-fail on the `docker`
		// driver. It never selects the builder as the default (no `buildx use`).
		fr := &scriptedRunner{
			fails: map[string]error{inspectFreeze: errors.New("no builder named aileron-freeze")},
			outputs: map[string]string{
				bootstrapFreeze: "Driver: docker-container\nPlatforms: linux/amd64, linux/arm64, linux/arm/v7\n",
			},
		}
		if err := CheckMultiArchBuild(context.Background(), fr, "docker"); err != nil {
			t.Fatalf("auto-provisioning a default setup must pass, got %v", err)
		}
		if n := fr.countCalls(createFreeze); n != 1 {
			t.Fatalf("create called %d times, want exactly 1 when the builder is absent", n)
		}
		for _, c := range fr.calls {
			if strings.HasPrefix(c, "buildx use") {
				t.Fatalf("preflight must not `buildx use` the operator's default builder, saw %q", c)
			}
		}
	})

	t.Run("bootstrap fails", func(t *testing.T) {
		fr := &scriptedRunner{fails: map[string]error{bootstrapFreeze: errors.New("failed to pull buildkit image")}}
		err := CheckMultiArchBuild(context.Background(), fr, "docker")
		if err == nil || !strings.Contains(err.Error(), binfmt) {
			t.Fatalf("err = %v, want a bootstrap-failure remediation", err)
		}
	})
}

// TestEnsureFreezeBuilder proves the dedicated freeze builder is created only
// when absent and skipped when already present, over the Runner seam (#2054).
func TestEnsureFreezeBuilder(t *testing.T) {
	const inspectFreeze = "buildx inspect " + FreezeBuilderName
	const createFreeze = "buildx create --name " + FreezeBuilderName + " --driver docker-container"

	t.Run("absent: created once with docker-container driver", func(t *testing.T) {
		fr := &scriptedRunner{fails: map[string]error{inspectFreeze: errors.New("no such builder")}}
		if err := EnsureFreezeBuilder(context.Background(), fr, "docker"); err != nil {
			t.Fatalf("EnsureFreezeBuilder: %v", err)
		}
		if n := fr.countCalls(createFreeze); n != 1 {
			t.Fatalf("create called %d times, want exactly 1", n)
		}
	})

	t.Run("present: create skipped", func(t *testing.T) {
		fr := &scriptedRunner{} // inspect succeeds by default
		if err := EnsureFreezeBuilder(context.Background(), fr, "docker"); err != nil {
			t.Fatalf("EnsureFreezeBuilder: %v", err)
		}
		if n := fr.countCalls(createFreeze); n != 0 {
			t.Fatalf("create called %d times, want 0 when the builder already exists", n)
		}
	})

	t.Run("create failure surfaces an actionable error", func(t *testing.T) {
		fr := &scriptedRunner{fails: map[string]error{
			inspectFreeze: errors.New("no such builder"),
			createFreeze:  errors.New("permission denied"),
		}}
		err := EnsureFreezeBuilder(context.Background(), fr, "docker")
		if err == nil || !strings.Contains(err.Error(), FreezeBuilderName) {
			t.Fatalf("err = %v, want a create-failure error naming the builder", err)
		}
	})
}

// scriptedRunner is a Runner whose Run is keyed on the joined args, writing the
// mapped stdout and returning the mapped error, so preflight checks run
// Docker-free. It records every joined-args key it saw so a test can assert which
// commands ran (e.g. that the freeze builder was created once, or skipped).
type scriptedRunner struct {
	outputs map[string]string
	fails   map[string]error
	calls   []string
}

func (s *scriptedRunner) Run(_ context.Context, _ string, args []string, stdout, _ io.Writer) error {
	key := strings.Join(args, " ")
	s.calls = append(s.calls, key)
	if err, ok := s.fails[key]; ok {
		return err
	}
	if out, ok := s.outputs[key]; ok {
		_, _ = stdout.Write([]byte(out))
	}
	return nil
}

// countCalls returns how many recorded calls equal key.
func (s *scriptedRunner) countCalls(key string) int {
	n := 0
	for _, c := range s.calls {
		if c == key {
			n++
		}
	}
	return n
}

func TestBuildDevcontainerWithFeaturesNoBuildArgs(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		Plan: composition.Plan{
			Tier:     composition.TierDevcontainer,
			Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, a := range runner.args {
		if a == "--build-arg" {
			t.Fatalf("args = %#v, want no --build-arg tokens", runner.args)
		}
	}
}

func TestBuildDevcontainerWithFeaturesNoDockerfileStillBuilds(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	runner := &recordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir:       dir,
		ToolchainMode: ToolchainModeHostNPX,
		Plan: composition.Plan{
			// No DockerfilePath: features-only plan must still build via the CLI
			// rather than returning ErrNoBuildRequired.
			Tier:     composition.TierDevcontainer,
			Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v, want a CLI build (not ErrNoBuildRequired)", err)
	}
	if !result.Built {
		t.Fatalf("result.Built = false, want true (features require a build)")
	}
	if runner.name != devcontainerCLI[0] {
		t.Fatalf("runner.name = %q, want %q", runner.name, devcontainerCLI[0])
	}
}

func TestBuildDevcontainerWithFeaturesNeverPolicySkipsExisting(t *testing.T) {
	dir := t.TempDir()
	writeFeaturesDevcontainer(t, dir)
	// image inspect succeeds (image present) so the never policy skips the build.
	runner := &callRecordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Policy:  BuildPolicyNever,
		Plan: composition.Plan{
			Tier:     composition.TierDevcontainer,
			Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Built {
		t.Fatalf("result.Built = true, want false (policy never with image present)")
	}
	// Only the image-inspect probe runs; no CLI build dispatch.
	want := []runnerCall{{name: "docker", args: []string{"image", "inspect", ProjectImageTag(dir)}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v (no devcontainer build)", runner.calls, want)
	}
}

func TestBuildDevcontainerWithoutFeaturesUsesDockerBuild(t *testing.T) {
	// Regression guard: the no-features Tier 1 path stays byte-for-byte
	// `docker build -t … -f … <dir>`.
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, ".devcontainer", "Dockerfile")
	if err := os.MkdirAll(filepath.Dir(dockerfile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: dir,
		Plan: composition.Plan{
			Tier:           composition.TierDevcontainer,
			DockerfilePath: "Dockerfile",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if runner.name != "docker" {
		t.Fatalf("runner.name = %q, want docker", runner.name)
	}
	want := []string{"build", "-t", ProjectImageTag(dir), "-f", dockerfile, dir}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

// synthesizedReadingRunner records the build args and, when a --workspace-folder
// is present, reads the devcontainer.json under it AT RUN TIME so the test can
// prove the synthesized config was materialized before the build dispatched (and
// is therefore gone after, once the deferred cleanup fires).
type synthesizedReadingRunner struct {
	name           string
	args           []string
	workspaceFlag  string
	devcontainerOK bool
	devcontainer   string
}

func (r *synthesizedReadingRunner) Run(_ context.Context, name string, args []string, _, _ io.Writer) error {
	r.name = name
	r.args = append([]string(nil), args...)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--workspace-folder" {
			r.workspaceFlag = args[i+1]
			b, err := os.ReadFile(filepath.Join(args[i+1], composition.DefaultDevcontainerPath))
			if err == nil {
				r.devcontainerOK = true
				r.devcontainer = string(b)
			}
		}
	}
	return nil
}

// TestBuildSynthesizedDevcontainerLocalAgentBuild is the #1451 regression: a
// Discover-synthesized plan (no .devcontainer on disk, a recipe'd-but-not-
// publishable agent like claude) must build LOCALLY via @devcontainers/cli from
// a materialized temp workspace folder — never the user's workDir — using the
// plan's deterministic local image tag rather than ProjectImageTag(workDir), and
// must leave the user's workDir untouched.
func TestBuildSynthesizedDevcontainerLocalAgentBuild(t *testing.T) {
	workDir := t.TempDir()
	const localTag = "aileron/sandbox-agent-claude:edge"
	synth := `{"name":"Aileron sandbox","image":"ghcr.io/alrubinger/aileron-sandbox-base:edge","features":{"ghcr.io/alrubinger/aileron-features/claude:0":{}}}`
	runner := &synthesizedReadingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir:       workDir,
		ToolchainMode: ToolchainModeHostNPX,
		Plan: composition.Plan{
			Tier:                    composition.TierDevcontainer,
			Image:                   localTag,
			Features:                map[string]json.RawMessage{"ghcr.io/alrubinger/aileron-features/claude:0": json.RawMessage("{}")},
			SynthesizedDevcontainer: synth,
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Built locally via the CLI, not pulled.
	if !result.Built {
		t.Fatalf("result.Built = false, want true (a local Feature build)")
	}
	if runner.name != devcontainerCLI[0] {
		t.Fatalf("runner.name = %q, want the devcontainer CLI %q", runner.name, devcontainerCLI[0])
	}
	// Image is the deterministic local tag, NOT ProjectImageTag(workDir).
	if result.Image != localTag {
		t.Fatalf("result.Image = %q, want the plan's local tag %q", result.Image, localTag)
	}
	if result.Image == ProjectImageTag(workDir) {
		t.Fatalf("result.Image must not be the workDir-keyed project tag")
	}
	if strings.Contains(result.Image, "aileron-sandbox-claude") {
		t.Fatalf("result.Image = %q must not reference a published claude image", result.Image)
	}
	// The build read a temp workspace folder, never the user's workDir.
	if runner.workspaceFlag == "" {
		t.Fatalf("no --workspace-folder passed to the CLI build")
	}
	if runner.workspaceFlag == workDir {
		t.Fatalf("--workspace-folder = workDir %q, want a managed temp folder", workDir)
	}
	if !runner.devcontainerOK {
		t.Fatalf("synthesized devcontainer.json was not materialized under the build workspace folder")
	}
	if runner.devcontainer != synth {
		t.Fatalf("materialized devcontainer = %q, want %q", runner.devcontainer, synth)
	}
	// The user's workDir must be untouched: no .devcontainer written there.
	if _, err := os.Stat(filepath.Join(workDir, composition.DefaultDevcontainerPath)); !os.IsNotExist(err) {
		t.Fatalf("user workDir was polluted with a .devcontainer (stat err=%v)", err)
	}
	// The temp workspace folder is cleaned up after Build returns.
	if _, err := os.Stat(runner.workspaceFlag); !os.IsNotExist(err) {
		t.Fatalf("temp build workspace %q was not cleaned up (stat err=%v)", runner.workspaceFlag, err)
	}
}

func TestBuildBYOImageWithFeaturesNoBuild(t *testing.T) {
	// Features on a BYO (Tier 2) image are inert; the build still short-circuits
	// to ErrNoBuildRequired with no CLI invocation.
	runner := &callRecordingRunner{errs: []error{errors.New("must not run")}}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Plan: composition.Plan{
			Tier:     composition.TierBYOImage,
			Image:    "ghcr.io/acme/agent:2026",
			Features: map[string]json.RawMessage{"./tool": json.RawMessage("{}")},
		},
	})
	if !errors.Is(err, ErrNoBuildRequired) {
		t.Fatalf("err = %v, want ErrNoBuildRequired", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls = %#v, want none (BYO features are inert)", runner.calls)
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
			Image: "aileron-sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Built {
		t.Fatalf("result.Built = true, want false")
	}
	want := []runnerCall{{name: "docker", args: []string{"image", "inspect", "aileron-sandbox-base:test"}}}
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
			Image: "aileron-sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !result.Built {
		t.Fatalf("result.Built = false, want true")
	}
	want := []runnerCall{
		{name: "docker", args: []string{"image", "inspect", "aileron-sandbox-base:test"}},
		{name: "docker", args: []string{"build", "-t", "aileron-sandbox-base:test", "-f", containerfile, filepath.Dir(containerfile)}},
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
			Image: "aileron-sandbox-base:test",
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
			Image: "aileron-sandbox-base:test",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Built {
		t.Fatalf("result.Built = true, want false")
	}
	want := []runnerCall{{name: "docker", args: []string{"image", "inspect", "aileron-sandbox-base:test"}}}
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
			Image: "aileron-sandbox-base:test",
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

const publishedImage = "ghcr.io/alrubinger/aileron-sandbox-claude:latest"

// publishedPinnedImage is a version-pinned published-image reference. Its tag is
// stable (the upstream digest never moves under a pinned tag), so a local copy
// may be cached under `auto` (issue #1174).
const publishedPinnedImage = "ghcr.io/alrubinger/aileron-sandbox-claude:v0.0.42"

// publishedEdgeImage carries the floating `edge` tag, whose upstream digest
// moves while the tag name stays fixed (issue #1174).
const publishedEdgeImage = "ghcr.io/alrubinger/aileron-sandbox-claude:edge"

func TestIsFloatingTag(t *testing.T) {
	cases := []struct {
		image string
		want  bool
	}{
		{"ghcr.io/alrubinger/aileron-sandbox-claude:latest", true},
		{"ghcr.io/alrubinger/aileron-sandbox-claude:edge", true},
		{"ghcr.io/alrubinger/aileron-sandbox-claude:v0.0.42", false},
		{"ghcr.io/alrubinger/aileron-sandbox-claude:0.0.42", false},
		// No tag segment defaults to latest → floating.
		{"ghcr.io/alrubinger/aileron-sandbox-claude", true},
		// A registry host:port prefix with no tag is not mistaken for a tag.
		{"localhost:5000/aileron-sandbox-claude", true},
		// A registry host:port prefix plus a pinned tag.
		{"localhost:5000/aileron-sandbox-claude:v1.2.3", false},
		// A registry host:port prefix plus a floating tag.
		{"localhost:5000/aileron-sandbox-claude:edge", true},
	}
	for _, c := range cases {
		if got := isFloatingTag(c.image); got != c.want {
			t.Errorf("isFloatingTag(%q) = %v, want %v", c.image, got, c.want)
		}
	}
}

func TestBuildPublishedAutoPullsMissingImage(t *testing.T) {
	// A pinned image absent locally → inspect (fails) → pull.
	runner := &callRecordingRunner{errs: []error{errors.New("missing"), nil}}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyAuto,
		Plan: composition.Plan{
			Tier:  composition.TierPublished,
			Image: publishedPinnedImage,
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Built {
		t.Fatalf("result.Built = true, want false (a pull is not a local build)")
	}
	if result.Image != publishedPinnedImage {
		t.Fatalf("result.Image = %q, want %q", result.Image, publishedPinnedImage)
	}
	if result.Tier != composition.TierPublished {
		t.Fatalf("result.Tier = %q, want %q", result.Tier, composition.TierPublished)
	}
	want := []runnerCall{
		{name: "docker", args: []string{"image", "inspect", publishedPinnedImage}},
		{name: "docker", args: []string{"pull", publishedPinnedImage}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestBuildPublishedAutoSkipsExistingPinnedImage(t *testing.T) {
	// A version-pinned tag is present locally → no pull (its digest never moves).
	runner := &callRecordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyAuto,
		Plan: composition.Plan{
			Tier:  composition.TierPublished,
			Image: publishedPinnedImage,
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Image != publishedPinnedImage {
		t.Fatalf("result.Image = %q, want %q", result.Image, publishedPinnedImage)
	}
	want := []runnerCall{{name: "docker", args: []string{"image", "inspect", publishedPinnedImage}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

// TestBuildPublishedAutoRepullsExistingFloatingImage is the issue #1174
// regression: a floating tag (edge/latest) that is already present locally must
// still pull under `auto`, because its upstream digest can have moved. The fix
// skips the existence short-circuit for floating tags, so no `image inspect`
// probe happens and the build goes straight to `pull`.
func TestBuildPublishedAutoRepullsExistingFloatingImage(t *testing.T) {
	for _, image := range []string{publishedImage, publishedEdgeImage} {
		t.Run(image, func(t *testing.T) {
			runner := &callRecordingRunner{}
			result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
				WorkDir: t.TempDir(),
				Policy:  BuildPolicyAuto,
				Plan: composition.Plan{
					Tier:  composition.TierPublished,
					Image: image,
				},
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if result.Built {
				t.Fatalf("result.Built = true, want false (a pull is not a local build)")
			}
			if result.Image != image {
				t.Fatalf("result.Image = %q, want %q", result.Image, image)
			}
			// Floating tags skip the inspect probe and pull unconditionally.
			want := []runnerCall{{name: "docker", args: []string{"pull", image}}}
			if !reflect.DeepEqual(runner.calls, want) {
				t.Fatalf("calls = %#v, want %#v", runner.calls, want)
			}
		})
	}
}

func TestBuildPublishedNeverSkipsExistingImage(t *testing.T) {
	runner := &callRecordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyNever,
		Plan: composition.Plan{
			Tier:  composition.TierPublished,
			Image: publishedImage,
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Image != publishedImage {
		t.Fatalf("result.Image = %q, want %q", result.Image, publishedImage)
	}
	want := []runnerCall{{name: "docker", args: []string{"image", "inspect", publishedImage}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestBuildPublishedNeverErrorsOnMissingImage(t *testing.T) {
	runner := &callRecordingRunner{errs: []error{errors.New("missing")}}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyNever,
		Plan: composition.Plan{
			Tier:  composition.TierPublished,
			Image: publishedImage,
		},
	})
	if err == nil {
		t.Fatal("expected missing image error")
	}
	if !strings.Contains(err.Error(), "sandbox build policy is never") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), publishedImage) {
		t.Fatalf("error %v does not mention the image %q", err, publishedImage)
	}
}

func TestBuildPublishedAlwaysPullsRegardlessOfPresence(t *testing.T) {
	runner := &callRecordingRunner{}
	result, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyAlways,
		Plan: composition.Plan{
			Tier:  composition.TierPublished,
			Image: publishedImage,
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Image != publishedImage {
		t.Fatalf("result.Image = %q, want %q", result.Image, publishedImage)
	}
	// always pulls without an inspect probe.
	want := []runnerCall{{name: "docker", args: []string{"pull", publishedImage}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestDetectDaemonUnreachable(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"empty", "", false},
		{"macos linux daemon", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock.", true},
		{"lowercase daemon", "cannot connect to the docker daemon", true},
		{"windows npipe api", "failed to connect to the docker API at npipe:////./pipe/docker_engine", true},
		{"windows file not found", "open //./pipe/docker_engine: The system cannot find the file specified.", true},
		{"manifest unknown is not unreachable", "Error response from daemon: manifest unknown", false},
		{"permission denied is not unreachable", "Got permission denied while trying to connect to the Docker daemon socket", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectDaemonUnreachable(tc.stderr); got != tc.want {
				t.Fatalf("detectDaemonUnreachable(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestBuildPullDaemonUnreachableFriendlyHeadline asserts that when a pull
// fails because the container daemon is unreachable, Build leads with the
// friendly "Docker isn't reachable" headline rather than the raw `exit status
// 1` transport error, while still chaining the underlying error so
// errors.Is/Unwrap exposes it for deeper diagnosis. Regression for #1496.
func TestBuildPullDaemonUnreachableFriendlyHeadline(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
	}{
		{
			name:   "windows npipe",
			stderr: "error during connect: Get \"http://%2F%2F.%2Fpipe%2Fdocker_engine/v1.45/...\": open //./pipe/docker_engine: The system cannot find the file specified.\nfailed to connect to the docker API at npipe:////./pipe/docker_engine\n",
		},
		{
			name:   "macos linux daemon",
			stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			underlying := errors.New("exit status 1")
			runner := &stderrWritingRunner{stderrText: tc.stderr, errReturn: underlying}
			_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
				WorkDir: t.TempDir(),
				Policy:  BuildPolicyAlways,
				Plan: composition.Plan{
					Tier:  composition.TierPublished,
					Image: publishedImage,
				},
			})
			if err == nil {
				t.Fatal("Build returned nil error, want daemon-unreachable failure")
			}
			if !strings.HasPrefix(err.Error(), daemonUnreachableMessage) {
				t.Fatalf("error headline = %q, want it to lead with %q", err.Error(), daemonUnreachableMessage)
			}
			if strings.HasPrefix(err.Error(), "exit status 1") {
				t.Fatalf("error leads with raw transport error: %q", err.Error())
			}
			// The technical error stays chained as a deeper diagnostic.
			if !errors.Is(err, underlying) {
				t.Fatalf("errors.Is(err, underlying) = false; underlying error not chained: %q", err.Error())
			}
			if !strings.Contains(err.Error(), "docker pull "+publishedImage) {
				t.Fatalf("error %q dropped the underlying `docker pull` framing", err.Error())
			}
		})
	}
}

// TestBuildPullErrorNotDaemonUnreachableKeepsVerbatimWrap asserts that a pull
// failure whose stderr does NOT match a daemon-unreachable signature keeps the
// established `<runtime> <args>: <err>` framing without the friendly headline.
func TestBuildPullErrorNotDaemonUnreachableKeepsVerbatimWrap(t *testing.T) {
	underlying := errors.New("exit status 1")
	runner := &stderrWritingRunner{
		stderrText: "Error response from daemon: manifest unknown\n",
		errReturn:  underlying,
	}
	_, err := Builder{Runtime: "docker", Runner: runner}.Build(context.Background(), BuildOptions{
		WorkDir: t.TempDir(),
		Policy:  BuildPolicyAlways,
		Plan: composition.Plan{
			Tier:  composition.TierPublished,
			Image: publishedImage,
		},
	})
	if err == nil {
		t.Fatal("Build returned nil error, want pull failure")
	}
	if strings.Contains(err.Error(), daemonUnreachableMessage) {
		t.Fatalf("non-daemon error gained the friendly headline: %q", err.Error())
	}
	if !errors.Is(err, underlying) {
		t.Fatalf("underlying error not chained: %q", err.Error())
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
			Image: "aileron-sandbox-base:test",
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
	if want := `unsupported sandbox runtime "nerdctl" (want auto or docker)`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestResolveRuntimeRejectsPodman is the regression test for #1051: v4
// is Docker-only, so an explicit podman runtime fails fast with the
// decided message regardless of whether podman is on PATH. The Runner
// seam and runtimeName parameterization are preserved for a future
// re-add, but resolution must reject podman today.
func TestResolveRuntimeRejectsPodman(t *testing.T) {
	const want = "podman runtime is not supported yet (v4 is Docker-only); see ADR-0014"
	// checkPath=true (PATH lookup would otherwise gate docker).
	if _, err := resolveRuntime("podman", true); err == nil || err.Error() != want {
		t.Fatalf("resolveRuntime(podman, true) error = %v, want %q", err, want)
	}
	// checkPath=false: still rejected even when path checks are skipped.
	if _, err := resolveRuntime("podman", false); err == nil || err.Error() != want {
		t.Fatalf("resolveRuntime(podman, false) error = %v, want %q", err, want)
	}
	// ResolveRuntime is the exported chokepoint used by the CLI paths.
	if _, err := ResolveRuntime("podman"); err == nil || err.Error() != want {
		t.Fatalf("ResolveRuntime(podman) error = %v, want %q", err, want)
	}
}

// TestResolveRuntimeAutoEmptyResolvesDocker pins that both "" and "auto"
// resolve to docker only (the candidate list dropped podman for #1051).
func TestResolveRuntimeAutoEmptyResolvesDocker(t *testing.T) {
	dir := t.TempDir()
	name := "docker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte{}, 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	for _, in := range []string{"", DefaultRuntime} {
		got, err := ResolveRuntime(in)
		if err != nil {
			t.Fatalf("ResolveRuntime(%q): %v", in, err)
		}
		if got != "docker" {
			t.Fatalf("ResolveRuntime(%q) = %q, want docker", in, got)
		}
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
	if want := "no container runtime found on PATH; install Docker, or pass --runtime"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
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
	_, err := baseBuildArgs(t.TempDir(), "aileron-sandbox-base:test", nil, "")
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
			Image: "aileron-sandbox-base:test",
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
		Image:   "aileron-sandbox-base:test",
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
		"run", "--rm",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"--env", "A_VAR=first",
		"--env", "Z_VAR=last",
		"aileron-sandbox-base:test",
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
		Image:   "aileron-sandbox-base:test",
		WorkDir: dir,
		User:    "root",
		Command: []string{"aileron-run-with-proxy-ca", "codex"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"run", "--rm",
		"--user", "root",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"aileron-sandbox-base:test",
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
		Image:   "aileron-sandbox-base:test",
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
		"run", "--rm",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"--volume", absExtra + ":/opt/aileron/manifests/actions:ro",
		"aileron-sandbox-base:test",
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
		Image:   "aileron-sandbox-base:test",
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

func TestRunMountsNamedVolumeVerbatim(t *testing.T) {
	dir := t.TempDir()
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:   "aileron-sandbox-base:test",
		WorkDir: dir,
		Volumes: []Volume{{
			Source: "aileron-cache-npm-deadbeef0123",
			Target: "/home/agent/.npm",
			Named:  true,
		}},
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A named volume must be emitted verbatim as <name>:<target>; filepath.Abs
	// must NOT run on it (that would mangle the volume name into a host path).
	wantSpec := "aileron-cache-npm-deadbeef0123:/home/agent/.npm"
	found := false
	for i := 0; i < len(runner.args)-1; i++ {
		if runner.args[i] == "--volume" && runner.args[i+1] == wantSpec {
			found = true
		}
		// Guard against the named volume being absolutized: no arg may contain
		// the workdir-joined form of the volume name.
		if strings.Contains(runner.args[i+1], filepath.Join(dir, "aileron-cache-npm-deadbeef0123")) {
			t.Fatalf("named volume was absolutized: %q", runner.args[i+1])
		}
	}
	if !found {
		t.Fatalf("named volume spec %q missing from args: %#v", wantSpec, runner.args)
	}
}

func TestRunMountsNamedVolumeReadOnly(t *testing.T) {
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image: "aileron-sandbox-base:test",
		Volumes: []Volume{{
			Source:   "aileron-cache-pip-abc123",
			Target:   "/cache",
			Named:    true,
			ReadOnly: true,
		}},
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantSpec := "aileron-cache-pip-abc123:/cache:ro"
	found := false
	for i := 0; i < len(runner.args)-1; i++ {
		if runner.args[i] == "--volume" && runner.args[i+1] == wantSpec {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("read-only named volume spec %q missing from args: %#v", wantSpec, runner.args)
	}
}

func TestRunRejectsIncompleteAdditionalVolume(t *testing.T) {
	_, err := Builder{Runtime: "docker", Runner: &recordingRunner{}}.Run(context.Background(), RunOptions{
		Image:   "aileron-sandbox-base:test",
		Volumes: []Volume{{Target: "/opt/aileron/manifests/actions"}},
		Command: []string{"codex"},
	})
	if err == nil {
		t.Fatal("expected incomplete volume error")
	}
}

func TestRunAddsTTYWhenRequested(t *testing.T) {
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:       "aileron-sandbox-base:test",
		Command:     []string{"claude"},
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(runner.args[:4], []string{"run", "--rm", "-i", "-t"}) {
		t.Fatalf("args prefix = %#v, want tty run prefix", runner.args[:4])
	}
}

// TestRunBatchOmitsInteractiveFlag is the regression guard for issue
// #1889: a default (batch) RunOptions must NOT emit docker's `-i`. The
// skill-launch path leaves Interactive false, and a batch `docker run
// -i` on a real terminal keeps stdin attached with no EOF so the run
// never returns and the caller hangs.
func TestRunBatchOmitsInteractiveFlag(t *testing.T) {
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:   "aileron-sandbox-base:test",
		Command: []string{"aileron", "skill", "run"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(runner.args[:2], []string{"run", "--rm"}) {
		t.Fatalf("args prefix = %#v, want [run --rm] with no -i", runner.args[:2])
	}
	for _, a := range runner.args {
		if a == "-i" {
			t.Fatalf("batch run args must not contain -i: %#v", runner.args)
		}
		if a == "-t" {
			t.Fatalf("batch run args must not contain -t: %#v", runner.args)
		}
	}
}

// TestRunInteractiveEmitsInteractiveFlag pins the interactive contract:
// an Interactive RunOptions (the agent-shell launch path) emits `-i` so
// the in-container process keeps stdin open.
func TestRunInteractiveEmitsInteractiveFlag(t *testing.T) {
	runner := &recordingRunner{}
	_, err := Builder{Runtime: "docker", Runner: runner}.Run(context.Background(), RunOptions{
		Image:       "aileron-sandbox-base:test",
		Command:     []string{"claude"},
		Interactive: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(runner.args[:3], []string{"run", "--rm", "-i"}) {
		t.Fatalf("args prefix = %#v, want [run --rm -i]", runner.args[:3])
	}
	for _, a := range runner.args {
		if a == "-t" {
			t.Fatalf("non-TTY interactive run must not contain -t: %#v", runner.args)
		}
	}
}

func TestInteractiveRun(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"run with -i", []string{"run", "--rm", "-i", "img", "claude"}, true},
		{"run without -i", []string{"run", "--rm", "img", "codex"}, false},
		{"exec with -i", []string{"exec", "-i", "c", "gh", "auth", "login"}, true},
		{"exec without -i", []string{"exec", "c", "gh", "auth", "token"}, false},
		{"build is never interactive", []string{"build", "-t", "tag", "."}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := interactiveRun(tc.args); got != tc.want {
				t.Fatalf("interactiveRun(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
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
		Image: "aileron-sandbox-base:test",
	})
	if err == nil {
		t.Fatal("expected missing command error")
	}
}

func TestValidateRunsMinimalContractProbe(t *testing.T) {
	pinHostOSDarwin(t)
	dir := t.TempDir()
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: dir,
		Env: map[string]string{
			"HTTPS_PROXY": "http://host.docker.internal:48123",
		},
		Command: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if runner.name != "docker" {
		t.Fatalf("runtime = %q, want docker", runner.name)
	}
	wantPrefix := []string{
		"run", "--rm",
		"--workdir", WorkspacePath,
		"--volume", dir + ":" + WorkspacePath,
		"--env", "HTTPS_PROXY=http://host.docker.internal:48123",
		"ghcr.io/acme/agent:latest",
		"/bin/sh", "-c",
	}
	if len(runner.args) < len(wantPrefix)+5 {
		t.Fatalf("args too short: %#v", runner.args)
	}
	if !reflect.DeepEqual(runner.args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("args prefix = %#v, want %#v", runner.args[:len(wantPrefix)], wantPrefix)
	}
	// After the shim slot was retired (#959) the trailing positional args
	// are: agent command ($1), proxy-trust flag ($2), MCP-binary flag ($3).
	if runner.args[len(runner.args)-3] != "codex" {
		t.Fatalf("validation command = %q, want codex", runner.args[len(runner.args)-3])
	}
	if runner.args[len(runner.args)-2] != "0" {
		t.Fatalf("proxy trust validation flag = %q, want 0", runner.args[len(runner.args)-2])
	}
	if runner.args[len(runner.args)-1] != "0" {
		t.Fatalf("mcp binary validation flag = %q, want 0", runner.args[len(runner.args)-1])
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
	if runner.args[len(runner.args)-3] != "codex" {
		t.Fatalf("validation command = %q, want codex", runner.args[len(runner.args)-3])
	}
	if runner.args[len(runner.args)-2] != "1" {
		t.Fatalf("proxy trust validation flag = %q, want 1", runner.args[len(runner.args)-2])
	}
	if runner.args[len(runner.args)-1] != "0" {
		t.Fatalf("mcp binary validation flag = %q, want 0", runner.args[len(runner.args)-1])
	}
	script := runner.args[len(runner.args)-5]
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

// TestValidateReportsStaleEntrypointContract reproduces the issue #1466 failure
// class: a remap-enabled validate prepends aileron-remap-agent-uid as the
// container command, but the image predates the entrypoint contract and lacks
// that helper, so the image's tini entrypoint fails to exec it (exit 127) with
// the tini "exec <prog> failed: No such file or directory" signature before the
// validation script runs. Validate must translate that into an actionable
// rebuild-edge message instead of surfacing the opaque tini error.
func TestValidateReportsStaleEntrypointContract(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
		// Verbatim tini diagnostic when PID 1 cannot exec its child.
		_, _ = stderr.Write([]byte("[FATAL tini (8)] exec aileron-remap-agent-uid failed: No such file or directory\n"))
		return errors.New("exit status 127")
	})
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:             "ghcr.io/alrubinger/aileron-sandbox-claude:edge",
		WorkDir:           t.TempDir(),
		Command:           []string{"claude"},
		RemapWorkspaceUID: true,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"validate sandbox image ghcr.io/alrubinger/aileron-sandbox-claude:edge",
		"predates this launcher's entrypoint contract",
		"aileron-remap-agent-uid",
		"Rebuild the base+agent edge images",
		AgentImagesDocsURL,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
	// The opaque tini line must not be the headline diagnostic; the actionable
	// remedy replaces it.
	if strings.Contains(msg, "[FATAL tini") {
		t.Fatalf("error should not surface the raw tini diagnostic, got %q", msg)
	}
}

// TestValidateStaleEntrypointSignatureRequiresRemap guards the gate: the
// stale-entrypoint translation only fires on the remap path. A non-remap
// validate that happens to see the helper name in stderr must fall through to
// the verbatim-detail path, not the rebuild-edge message.
func TestValidateStaleEntrypointSignatureRequiresRemap(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
		_, _ = stderr.Write([]byte("exec aileron-remap-agent-uid failed: No such file or directory\n"))
		return errors.New("exit status 127")
	})
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Image:   "ghcr.io/acme/agent:latest",
		WorkDir: t.TempDir(),
		Command: []string{"codex"},
		// RemapWorkspaceUID intentionally false.
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if strings.Contains(err.Error(), "predates this launcher's entrypoint contract") {
		t.Fatalf("non-remap validate must not emit the stale-entrypoint message, got %q", err.Error())
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

func TestValidate_RequireMCPBinary_AppendsThirdPositional(t *testing.T) {
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
	script := runner.args[len(runner.args)-5]
	for _, want := range []string{
		`if [ "${3:-0}" = "1" ]; then`,
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
	err := StopContainer(context.Background(), runner, "docker", "aileron-sbx-y", 10, io.Discard, io.Discard)
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

// TestRunArgs_LinuxNonDockerOmitsHostGateway exercises the negative
// branch of the runtime seam: the --add-host injection is gated on
// runtimeName == "docker", so any other runtime threaded through the
// preserved runtimeName parameter omits it. v4 is Docker-only, but the
// gate must stay runtime-aware for a future re-added runtime.
func TestRunArgs_LinuxNonDockerOmitsHostGateway(t *testing.T) {
	orig := hostOS
	hostOS = func() string { return "linux" }
	defer func() { hostOS = orig }()

	args, err := runArgs("nondocker", runOptsForGateway(t))
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, a := range args {
		if a == "host.docker.internal:host-gateway" {
			t.Errorf("did not expect --add-host on Linux non-docker runtime; got %v", args)
		}
	}
}

// --- runArgs SELinux :z workspace relabel (#1458) ---

// pinSELinux overrides the package-level hostOS and selinuxEnforcing seams so
// the relabel argv branch can be exercised on any test runner.
func pinSELinux(t *testing.T, os string, enforcing bool) {
	t.Helper()
	origOS := hostOS
	origSE := selinuxEnforcing
	hostOS = func() string { return os }
	selinuxEnforcing = func() bool { return enforcing }
	t.Cleanup(func() {
		hostOS = origOS
		selinuxEnforcing = origSE
	})
}

// workspaceMountSpec returns the value following the first "--volume" flag,
// which runArgs always emits as the workspace bind mount.
func workspaceMountSpec(t *testing.T, args []string) string {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--volume" {
			return args[i+1]
		}
	}
	t.Fatalf("no --volume found in args: %v", args)
	return ""
}

func TestRunArgs_LinuxSELinuxEnforcingRelabelsWorkspace(t *testing.T) {
	pinSELinux(t, "linux", true)
	opts := runOptsForGateway(t)
	args, err := runArgs("docker", opts)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	spec := workspaceMountSpec(t, args)
	if !strings.HasSuffix(spec, ":"+WorkspacePath+":z") {
		t.Errorf("workspace mount %q missing :z relabel suffix under SELinux enforcing", spec)
	}
}

func TestRunArgs_LinuxSELinuxEnforcingRelabelsHostVolumes(t *testing.T) {
	pinSELinux(t, "linux", true)
	dir := t.TempDir()
	extra := filepath.Join(dir, "actions")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	args, err := runArgs("docker", RunOptions{
		Image:   "img",
		WorkDir: dir,
		Command: []string{"sh"},
		Volumes: []Volume{
			{Source: extra, Target: "/opt/aileron/manifests/actions", ReadOnly: true},
			{Source: "aileron-cache-npm", Target: "/home/agent/.npm", Named: true},
		},
	})
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	absExtra, _ := filepath.Abs(extra)
	joined := strings.Join(args, " ")
	// A read-only host bind mount keeps ro and gains z, joined as a single
	// comma-separated options field (Docker rejects ro:z as a fourth field).
	if !strings.Contains(joined, "--volume "+absExtra+":/opt/aileron/manifests/actions:ro,z") {
		t.Errorf("host bind mount missing ro,z options under SELinux enforcing; got %v", args)
	}
	// A named volume is runtime-managed and must NOT be relabeled.
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--volume" && strings.HasPrefix(args[i+1], "aileron-cache-npm:") {
			if strings.HasSuffix(args[i+1], ":z") {
				t.Errorf("named volume must not be relabeled; got %q", args[i+1])
			}
		}
	}
}

func TestRunArgs_DarwinOmitsRelabel(t *testing.T) {
	pinSELinux(t, "darwin", true) // enforcing forced true, but non-Linux must skip
	opts := runOptsForGateway(t)
	args, err := runArgs("docker", opts)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	spec := workspaceMountSpec(t, args)
	if strings.HasSuffix(spec, ":z") {
		t.Errorf("workspace mount %q must not be relabeled on darwin", spec)
	}
}

func TestRunArgs_LinuxNonEnforcingOmitsRelabel(t *testing.T) {
	pinSELinux(t, "linux", false)
	opts := runOptsForGateway(t)
	args, err := runArgs("docker", opts)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	spec := workspaceMountSpec(t, args)
	if strings.HasSuffix(spec, ":z") {
		t.Errorf("workspace mount %q must not be relabeled when SELinux is not enforcing", spec)
	}
}

func TestRunArgs_LinuxNonDockerOmitsRelabel(t *testing.T) {
	pinSELinux(t, "linux", true)
	opts := runOptsForGateway(t)
	args, err := runArgs("nondocker", opts)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	spec := workspaceMountSpec(t, args)
	if strings.HasSuffix(spec, ":z") {
		t.Errorf("workspace mount %q must not be relabeled on a non-docker runtime", spec)
	}
}

// --- Workspace uid-remap gate + Validate routing (#1461) ---

func TestWorkspaceUIDRemapActive(t *testing.T) {
	cases := []struct {
		name    string
		os      string
		runtime string
		want    bool
	}{
		{"linux docker remaps", "linux", "docker", true},
		{"darwin docker skips", "darwin", "docker", false},
		{"windows docker skips", "windows", "docker", false},
		{"linux non-docker skips", "linux", "podman", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// enforcing is irrelevant to the remap gate; pin it false.
			pinSELinux(t, tc.os, false)
			if got := WorkspaceUIDRemapActive(tc.runtime); got != tc.want {
				t.Errorf("WorkspaceUIDRemapActive(%q) on %s = %v, want %v", tc.runtime, tc.os, got, tc.want)
			}
		})
	}
}

func TestWorkspaceRelabelActive(t *testing.T) {
	cases := []struct {
		name      string
		os        string
		enforcing bool
		runtime   string
		want      bool
	}{
		{"linux docker enforcing relabels", "linux", true, "docker", true},
		{"linux docker non-enforcing skips", "linux", false, "docker", false},
		{"darwin docker enforcing skips", "darwin", true, "docker", false},
		{"linux non-docker enforcing skips", "linux", true, "podman", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinSELinux(t, tc.os, tc.enforcing)
			if got := WorkspaceRelabelActive(tc.runtime); got != tc.want {
				t.Errorf("WorkspaceRelabelActive(%q) os=%s enforcing=%v = %v, want %v", tc.runtime, tc.os, tc.enforcing, got, tc.want)
			}
		})
	}
}

// TestValidateRoutesThroughRemapWhenRequested asserts that with
// RemapWorkspaceUID set, Validate runs the container as root and prepends the
// aileron-remap-agent-uid + su-exec chain so the writability probe sees the
// remapped agent identity (issue #1461). Without it, the probe runs as the
// image default user with no wrapper.
func TestValidateRoutesThroughRemapWhenRequested(t *testing.T) {
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Runtime:           "docker",
		Image:             "img",
		Command:           []string{"claude"},
		RemapWorkspaceUID: true,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	joined := strings.Join(runner.args, " ")
	if !strings.Contains(joined, "--user root") {
		t.Errorf("remap Validate must run as root; args: %v", runner.args)
	}
	// The remap helper, su-exec, agent, then the validation shell must appear
	// in order before the image, with the remap chain immediately preceding
	// /bin/sh.
	if !strings.Contains(joined, "aileron-remap-agent-uid su-exec agent /bin/sh -c") {
		t.Errorf("remap Validate must chain remap->su-exec->agent->/bin/sh; args: %v", runner.args)
	}
}

func TestValidateNoRemapByDefault(t *testing.T) {
	runner := &recordingRunner{}
	err := Builder{Runtime: "docker", Runner: runner}.Validate(context.Background(), ValidateOptions{
		Runtime: "docker",
		Image:   "img",
		Command: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	joined := strings.Join(runner.args, " ")
	if strings.Contains(joined, "--user root") {
		t.Errorf("default Validate must not force root; args: %v", runner.args)
	}
	if strings.Contains(joined, "aileron-remap-agent-uid") {
		t.Errorf("default Validate must not prepend the remap helper; args: %v", runner.args)
	}
}

// --- BakedMCPVersion image-label detection (U3 / #957) ---

func TestBakedMCPVersion(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		runErr error
		want   string
	}{
		{name: "baked", stdout: "0.0.42\n", want: "0.0.42"},
		{name: "trailing and leading whitespace", stdout: "  0.0.42  \n", want: "0.0.42"},
		{name: "unlabeled empty", stdout: "\n", want: ""},
		{name: "no labels sentinel", stdout: "<no value>\n", want: ""},
		{name: "inspect error not baked", stdout: "", runErr: errors.New("no such image"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := runnerFunc(func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
				if tc.runErr != nil {
					return tc.runErr
				}
				_, _ = io.WriteString(stdout, tc.stdout)
				return nil
			})
			got := BakedMCPVersion(context.Background(), runner, "docker", "img:test")
			if got != tc.want {
				t.Fatalf("BakedMCPVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBakedMCPVersion_InspectArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := runnerFunc(func(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
		gotName = name
		gotArgs = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "0.0.1\n")
		return nil
	})
	BakedMCPVersion(context.Background(), runner, "docker", "ghcr.io/acme/base:latest")
	if gotName != "docker" {
		t.Fatalf("runtime name = %q, want docker", gotName)
	}
	want := []string{
		"image", "inspect",
		"--format", `{{ index .Config.Labels "ai.aileron.mcp.version" }}`,
		"ghcr.io/acme/base:latest",
	}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("inspect args = %v, want %v", gotArgs, want)
	}
}

// --- BakedCLIVersion ai.aileron.cli.version detection (#1809) ---

func TestBakedCLIVersion(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		runErr error
		want   string
	}{
		{name: "baked", stdout: "0.0.42\n", want: "0.0.42"},
		{name: "trailing and leading whitespace", stdout: "  0.0.42  \n", want: "0.0.42"},
		{name: "unlabeled empty", stdout: "\n", want: ""},
		{name: "no labels sentinel", stdout: "<no value>\n", want: ""},
		{name: "inspect error not baked", stdout: "", runErr: errors.New("no such image"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := runnerFunc(func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
				if tc.runErr != nil {
					return tc.runErr
				}
				_, _ = io.WriteString(stdout, tc.stdout)
				return nil
			})
			got := BakedCLIVersion(context.Background(), runner, "docker", "img:test")
			if got != tc.want {
				t.Fatalf("BakedCLIVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBakedCLIVersion_InspectArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := runnerFunc(func(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
		gotName = name
		gotArgs = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "0.0.1\n")
		return nil
	})
	BakedCLIVersion(context.Background(), runner, "docker", "ghcr.io/acme/base:latest")
	if gotName != "docker" {
		t.Fatalf("runtime name = %q, want docker", gotName)
	}
	want := []string{
		"image", "inspect",
		"--format", `{{ index .Config.Labels "ai.aileron.cli.version" }}`,
		"ghcr.io/acme/base:latest",
	}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("inspect args = %v, want %v", gotArgs, want)
	}
}

// TestBakedCLIVersion_NilRunnerDefaults proves a nil Runner degrades to the
// production exec runner rather than panicking. The inspect targets a
// guaranteed-absent image so the exec path returns an error and the function
// fail-softs to "".
func TestBakedCLIVersion_NilRunnerDefaults(t *testing.T) {
	got := BakedCLIVersion(context.Background(), nil, DefaultRuntime,
		"aileron-nonexistent-image-for-test:doesnotexist")
	if got != "" {
		t.Fatalf("BakedCLIVersion(nil runner, absent image) = %q, want \"\"", got)
	}
}

// --- ImageMetadataLabel devcontainer.metadata detection (#1322) ---

func TestImageMetadataLabel(t *testing.T) {
	const metadataJSON = `[{"id":"gh","customizations":{"aileron":{"cli":{"name":"gh"}}}}]`
	cases := []struct {
		name   string
		stdout string
		runErr error
		want   string
	}{
		{name: "metadata present", stdout: metadataJSON + "\n", want: metadataJSON},
		{name: "trailing and leading whitespace", stdout: "  " + metadataJSON + "  \n", want: metadataJSON},
		{name: "unlabeled empty", stdout: "\n", want: ""},
		{name: "no labels sentinel", stdout: "<no value>\n", want: ""},
		{name: "inspect error no metadata", stdout: "", runErr: errors.New("no such image"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := runnerFunc(func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
				if tc.runErr != nil {
					return tc.runErr
				}
				_, _ = io.WriteString(stdout, tc.stdout)
				return nil
			})
			got := ImageMetadataLabel(context.Background(), runner, "docker", "img:test")
			if got != tc.want {
				t.Fatalf("ImageMetadataLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImageMetadataLabel_InspectArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := runnerFunc(func(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
		gotName = name
		gotArgs = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "[]\n")
		return nil
	})
	ImageMetadataLabel(context.Background(), runner, "docker", "ghcr.io/acme/base:latest")
	if gotName != "docker" {
		t.Fatalf("runtime name = %q, want docker", gotName)
	}
	want := []string{
		"image", "inspect",
		"--format", `{{ index .Config.Labels "devcontainer.metadata" }}`,
		"ghcr.io/acme/base:latest",
	}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("inspect args = %v, want %v", gotArgs, want)
	}
}

// TestImageMetadataLabel_NilRunnerDefaults proves a nil Runner degrades to
// the production exec runner rather than panicking. The inspect targets a
// guaranteed-absent image so the exec path returns an error and the function
// fail-softs to "".
func TestImageMetadataLabel_NilRunnerDefaults(t *testing.T) {
	got := ImageMetadataLabel(context.Background(), nil, DefaultRuntime,
		"aileron-nonexistent-image-for-test:doesnotexist")
	if got != "" {
		t.Fatalf("ImageMetadataLabel(nil runner, absent image) = %q, want \"\"", got)
	}
}
