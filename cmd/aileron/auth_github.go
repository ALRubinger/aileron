package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"

	"golang.org/x/term"
)

// stdinIsTerminal reports whether the operator's stdin is a real
// terminal. It is a package var so tests can drive both branches.
// gh's interactive device-flow login renders a prompt through a
// prompt library that requires a pseudo-TTY; the login exec therefore
// needs `docker exec -t`. We gate on a real stdin so a non-TTY / CI
// caller does not hit docker's "cannot enable tty mode on non tty
// input" — they get a plain `exec -i` instead (which will not complete
// an interactive login, but fails cleanly rather than mis-allocating a
// PTY).
var stdinIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// deviceFlowRunner performs gh's OAuth device-authorization flow and
// returns the captured user-to-server bearer token bytes. It is a seam
// so tests can substitute a fake without driving a real container or
// reaching GitHub. The production implementation is containerDeviceFlow.
type deviceFlowRunner interface {
	// Capture drives the device flow and returns the raw bearer token
	// (trailing newline trimmed). It is interactive: the operator reads
	// the user code + verification URL gh prints, authorizes in a
	// browser, and gh completes the grant.
	Capture(ctx context.Context) ([]byte, error)
}

// newDeviceFlowRunner builds the production runner. It is a package var
// so runAuthGitHub's tests substitute a fake flow without a container.
var newDeviceFlowRunner = func(runtime, image string) (deviceFlowRunner, error) {
	runtimeExe, err := sandboxcontainer.ResolveRuntime(runtime)
	if err != nil {
		return nil, err
	}
	image = resolveDeviceFlowImage(image)
	return &containerDeviceFlow{
		runner:        sandboxcontainer.DefaultRunner(),
		runtimeExe:    runtimeExe,
		image:         image,
		containerName: "aileron-auth-github",
	}, nil
}

// resolveDeviceFlowImage returns the container image for the device
// flow: the caller's --image override when set, otherwise the sandbox
// base image. The base image ships gh (#1146) and is agent-independent,
// which matches the user-level (not per-agent) nature of this
// credential. edge for dev builds, latest for releases (#1141).
func resolveDeviceFlowImage(override string) string {
	if override != "" {
		return override
	}
	return sandboxcomposition.BaseImage(version.Version)
}

// runAuthGitHub implements `aileron auth github`. It drives gh's OAuth
// device flow inside a container that ships gh, captures the resulting
// non-expiring bearer token, and PUTs it to the user/github vault path
// through the daemon. OAuth is acquisition UX only: a single bearer
// token is stored, no refresh machinery, HTTPS only.
func runAuthGitHub(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("auth github", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtime := flags.String("runtime", sandboxcontainer.DefaultRuntime,
		"Container runtime: auto or docker")
	image := flags.String("image", "",
		"Override the container image (defaults to the gh-bearing sandbox base image)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron auth github [--runtime <auto|docker>] [--image <ref>]")
		return 1
	}

	runner, err := newDeviceFlowRunner(*runtime, *image)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	token, err := runner.Capture(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "error: GitHub device-flow login failed: %v\n", err)
		return 1
	}
	if len(bytes.TrimSpace(token)) == 0 {
		fmt.Fprintln(stderr, "error: gh returned an empty token; login did not complete")
		return 1
	}

	// Marshaling a struct with a single []byte field cannot fail; the
	// error is discarded here matching runAuthImport's precedent.
	body, _ := json.Marshal(agentCredentialsBody{Value: token})
	status, respBody, err := vaultDoRequest(http.MethodPut,
		"/vault/user/github/credentials", body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch status {
	case http.StatusNoContent:
		fmt.Fprintln(stdout, "Stored user/github")
		return 0
	case http.StatusLocked:
		fmt.Fprintln(stderr, "error: vault is locked; unlock it first")
		return 1
	case http.StatusServiceUnavailable:
		fmt.Fprintln(stderr, "error: daemon is not configured with a vault")
		return 1
	default:
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
}

// containerDeviceFlow drives gh's device flow inside one persistent
// container so the token gh auth login writes to ~/.config/gh/hosts.yml
// survives to the gh auth token read. It runs the SAME named container
// for both gh calls: an ephemeral second `docker run` would not see the
// hosts.yml the login wrote, so the shared container is load-bearing.
type containerDeviceFlow struct {
	runner        sandboxcontainer.Runner
	runtimeExe    string // the resolved runtime executable, e.g. "docker"
	image         string
	containerName string
}

// Capture starts the container, runs the interactive gh auth login
// (device flow), reads the token with gh auth token from that same
// container, then tears the container down. The teardown is deferred so
// a mid-flow failure still removes the container.
func (c *containerDeviceFlow) Capture(ctx context.Context) ([]byte, error) {
	// Clear any container left behind by a prior run that died before its
	// teardown (e.g. SIGKILL): the name is deterministic, so a stale
	// container would make `run --name` fail with "name already in use".
	// rm -f on a nonexistent name is a harmless no-op, so the error is
	// ignored.
	_ = c.runner.Run(ctx, c.runtimeExe,
		[]string{"rm", "-f", c.containerName}, io.Discard, io.Discard)

	// Start one long-lived container we exec into twice. --rm is not used
	// because we explicitly `rm -f` in the deferred teardown; sleep keeps
	// it alive between the login and token execs.
	var startErr bytes.Buffer
	runArgs := []string{
		"run", "-d", "--name", c.containerName,
		c.image, "sleep", "3600",
	}
	if err := c.runner.Run(ctx, c.runtimeExe, runArgs, io.Discard, &startErr); err != nil {
		return nil, fmt.Errorf("starting container: %w%s", err, stderrSuffix(&startErr))
	}
	// Tear the container down regardless of how Capture exits.
	defer func() {
		_ = c.runner.Run(context.Background(), c.runtimeExe,
			[]string{"rm", "-f", c.containerName}, io.Discard, io.Discard)
	}()

	// Interactive device-flow login. gh prints the user code + the
	// verification URL to the operator's terminal; its stdin/stdout are
	// wired to the host terminal by the Runner so the operator can read
	// the prompt. HTTPS only — never configure SSH (-p https). Output is
	// surfaced so the operator sees gh's instructions.
	//
	// gh's prompt needs a pseudo-TTY, so the exec gets `-t` when stdin
	// is a real terminal. Without it `gh auth login --web` hangs with no
	// visible prompt (the prompt library never renders). `-i` and `-t`
	// are passed as separate args so the Runner's interactiveTTYRun gate
	// matches `-t` and hands the child the controlling terminal's
	// foreground process group — required because the Runner isolates
	// the docker child into its own process group, where docker's
	// raw-mode tcsetattr would otherwise fail with SIGTTOU/EINTR.
	loginArgs := []string{"exec", "-i"}
	if stdinIsTerminal() {
		loginArgs = append(loginArgs, "-t")
	}
	loginArgs = append(loginArgs,
		c.containerName,
		"gh", "auth", "login",
		"--hostname", "github.com",
		"--git-protocol", "https",
		"--web",
	)
	if err := c.runner.Run(ctx, c.runtimeExe, loginArgs, os.Stdout, os.Stderr); err != nil {
		return nil, fmt.Errorf("gh auth login: %w", err)
	}

	// Read the token gh auth login wrote to hosts.yml in THIS container.
	var tokenOut, tokenErr bytes.Buffer
	tokenArgs := []string{
		"exec", c.containerName,
		"gh", "auth", "token", "--hostname", "github.com",
	}
	if err := c.runner.Run(ctx, c.runtimeExe, tokenArgs, &tokenOut, &tokenErr); err != nil {
		return nil, fmt.Errorf("gh auth token: %w%s", err, stderrSuffix(&tokenErr))
	}
	return bytes.TrimRight(tokenOut.Bytes(), "\r\n"), nil
}

// stderrSuffix renders captured stderr as a trailing context fragment
// for an error message, or "" when there is none.
func stderrSuffix(b *bytes.Buffer) string {
	s := bytes.TrimSpace(b.Bytes())
	if len(s) == 0 {
		return ""
	}
	return ": " + string(s)
}
