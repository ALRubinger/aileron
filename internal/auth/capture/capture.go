// Package capture is a provider-independent substrate for acquiring a
// credential by driving an interactive CLI login inside a container and
// reading the produced token back out.
//
// It is the tool-agnostic core extracted from the bespoke
// `aileron auth github` device-flow capture (#1286). All tool-specific
// knowledge — the container image, the container name, the login arg
// vector, the token-read arg vector, the config-dir env, the optional
// BROWSER shim, the vault path, and the kind metadata stamp — is a
// parameter on Driver. Nothing about any particular CLI (gh, github.com,
// user/github, --web, …) is compiled into this package.
//
// The driver runs the SAME named container for both execs: an
// interactive login writes the tool's credential into the container's
// config home, and a second exec reads it back. An ephemeral second
// container would not see what the login wrote, so the shared container
// is load-bearing — the lifecycle (clear stale name, start, login, read,
// teardown-on-every-exit) is the substrate this package owns.
//
// The vault PUT is NOT owned here. `internal` cannot import `cmd`, where
// the daemon vault transport lives, so storage is expressed as an
// injected StoreFunc seam. The driver stamps the captured bytes with the
// configured kind and hands them to Store; the HTTP status → message
// mapping stays caller-side (#1288). The driver surfaces the store
// error verbatim.
package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"

	"golang.org/x/term"
)

// ErrEmptyToken is returned by Acquire when the login exec completes but
// the token read yields only whitespace. The token is never stored in
// that case: an empty credential would fail closed at the binding
// boundary, so the driver reports the incomplete login rather than
// persisting a useless entry.
var ErrEmptyToken = errors.New("capture: token read returned an empty value; login did not complete")

// StoreFunc persists the captured credential. It is the seam that keeps
// the driver transport-agnostic: production wiring supplies a closure
// over the daemon vault PUT (#1288); tests supply a recording fake.
//
// storeAt is the vault path, kind is the metadata Type stamp, and value
// is the trimmed credential bytes. The returned error is surfaced
// verbatim by Acquire — the caller owns any status → message mapping.
type StoreFunc func(ctx context.Context, storeAt, kind string, value []byte) error

// stdinIsTerminal reports whether the operator's stdin is a real
// terminal. It is a package var so tests can drive both branches.
//
// An interactive CLI login (e.g. a device-flow prompt) typically renders
// through a prompt library that requires a pseudo-TTY, so the login exec
// needs `-t`. We gate on a real stdin so a non-TTY / CI caller does not
// hit the runtime's "cannot enable tty mode on non tty input" — they get
// a plain `exec -i` instead (which will not complete an interactive
// login, but fails cleanly rather than mis-allocating a PTY).
var stdinIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// Driver captures a credential by driving an interactive CLI login
// inside a container and reading the produced token back out. Every
// tool-specific value is a field; the zero Driver is not usable — Runner,
// RuntimeExe, Image, ContainerName, LoginArgs, TokenArgs, StoreAt, and
// Store must be set.
type Driver struct {
	// Runner executes runtime commands. Defaults via New to
	// sandboxcontainer.DefaultRunner(); tests inject a fake.
	Runner sandboxcontainer.Runner
	// RuntimeExe is the resolved runtime executable, e.g. "docker".
	RuntimeExe string
	// Image is the fully-resolved container image to run. Image / base-image
	// resolution policy stays in the caller; the driver takes a ready string.
	Image string
	// ContainerName is the deterministic name for the long-lived container.
	ContainerName string

	// LoginArgs is the interactive login command run inside the container,
	// minus the exec scaffolding — e.g. {"gh","auth","login","--web"}. The
	// driver prepends exec/-i/-t/--env tokens and the container name.
	LoginArgs []string
	// TokenArgs is the token-read command run inside the same container,
	// e.g. {"gh","auth","token"}. The driver prepends `exec <name>`.
	TokenArgs []string

	// ConfigDirEnv, when non-empty, is passed as a single `--env=K=V` token
	// (e.g. "GH_CONFIG_DIR=/path") on BOTH the login and token execs so the
	// token read sees the same config home the login wrote to. Empty omits it.
	ConfigDirEnv string
	// BrowserShim, when non-empty, is passed as `--env=BROWSER=<shim>` on the
	// login exec only, to turn a missing-browser open into a clean no-op.
	// Empty omits it. The non-interactive token read never carries it.
	BrowserShim string

	// StoreAt is the vault path the captured credential is stored at.
	StoreAt string
	// Kind is the metadata Type stamped on the stored credential.
	Kind string
	// Store persists the credential. Required.
	Store StoreFunc
}

// New constructs a Driver with the runtime resolved and the Runner
// defaulted to the production exec runner, mirroring the github flow's
// constructor. Image/base-image resolution stays caller-side: pass a
// fully-resolved image. The returned Driver still needs LoginArgs,
// TokenArgs, ContainerName, StoreAt, Kind, and Store set by the caller.
func New(runtime, image string) (*Driver, error) {
	runtimeExe, err := sandboxcontainer.ResolveRuntime(runtime)
	if err != nil {
		return nil, err
	}
	return &Driver{
		Runner:     sandboxcontainer.DefaultRunner(),
		RuntimeExe: runtimeExe,
		Image:      image,
	}, nil
}

// Capture starts the container, runs the interactive login, reads the
// token from that same container, then tears the container down. The
// teardown is deferred so a mid-flow failure still removes the container.
// The returned bytes are the token with trailing CR/LF trimmed.
func (d *Driver) Capture(ctx context.Context) ([]byte, error) {
	// Clear any container left behind by a prior run that died before its
	// teardown (e.g. SIGKILL): the name is deterministic, so a stale
	// container would make `run --name` fail with "name already in use".
	// rm -f on a nonexistent name is a harmless no-op, so the error is
	// ignored.
	_ = d.Runner.Run(ctx, d.RuntimeExe,
		[]string{"rm", "-f", d.ContainerName}, io.Discard, io.Discard)

	// Start one long-lived container we exec into twice. --rm is not used
	// because we explicitly `rm -f` in the deferred teardown; sleep keeps
	// it alive between the login and token execs.
	var startErr bytes.Buffer
	runArgs := []string{
		"run", "-d", "--name", d.ContainerName,
		d.Image, "sleep", "3600",
	}
	if err := d.Runner.Run(ctx, d.RuntimeExe, runArgs, io.Discard, &startErr); err != nil {
		return nil, fmt.Errorf("starting container: %w%s", err, stderrSuffix(&startErr))
	}
	// Tear the container down regardless of how Capture exits.
	defer func() {
		_ = d.Runner.Run(context.Background(), d.RuntimeExe,
			[]string{"rm", "-f", d.ContainerName}, io.Discard, io.Discard)
	}()

	// Interactive login. The login's stdin/stdout are wired to the host
	// terminal by the Runner so the operator can read any prompt.
	//
	// The login exec gets `-t` when stdin is a real terminal so a
	// prompt-library login renders. `-i` and `-t` are passed as separate
	// args so the Runner's interactiveTTYRun gate matches `-t` and hands
	// the child the controlling terminal's foreground process group —
	// required because the Runner isolates the runtime child into its own
	// process group, where raw-mode tcsetattr would otherwise fail with
	// SIGTTOU/EINTR.
	loginArgs := []string{"exec", "-i"}
	if stdinIsTerminal() {
		loginArgs = append(loginArgs, "-t")
	}
	// The browser shim, when set, makes a missing-browser open a clean
	// no-op. Passed as a single `--env=K=V` token so it stays a
	// `-`-prefixed exec flag (the runtime's arg parsing skips those).
	if d.BrowserShim != "" {
		loginArgs = append(loginArgs, "--env=BROWSER="+d.BrowserShim)
	}
	// The config-dir env is on BOTH execs so the token read sees the same
	// config home the login wrote to.
	if d.ConfigDirEnv != "" {
		loginArgs = append(loginArgs, "--env="+d.ConfigDirEnv)
	}
	loginArgs = append(loginArgs, d.ContainerName)
	loginArgs = append(loginArgs, d.LoginArgs...)
	if err := d.Runner.Run(ctx, d.RuntimeExe, loginArgs, os.Stdout, os.Stderr); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}

	// Read the token the login wrote into the config home of THIS container.
	var tokenOut, tokenErr bytes.Buffer
	tokenArgs := []string{"exec"}
	if d.ConfigDirEnv != "" {
		tokenArgs = append(tokenArgs, "--env="+d.ConfigDirEnv)
	}
	tokenArgs = append(tokenArgs, d.ContainerName)
	tokenArgs = append(tokenArgs, d.TokenArgs...)
	if err := d.Runner.Run(ctx, d.RuntimeExe, tokenArgs, &tokenOut, &tokenErr); err != nil {
		return nil, fmt.Errorf("reading token: %w%s", err, stderrSuffix(&tokenErr))
	}
	return bytes.TrimRight(tokenOut.Bytes(), "\r\n"), nil
}

// Acquire is the full driver entrypoint: it captures the token, guards
// the empty-after-trim case (no store, ErrEmptyToken), then stamps the
// configured kind and hands the bytes to Store. Store's error is
// returned verbatim. Store is never called on a capture failure or an
// empty token.
func (d *Driver) Acquire(ctx context.Context) error {
	token, err := d.Capture(ctx)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(token)) == 0 {
		return ErrEmptyToken
	}
	return d.Store(ctx, d.StoreAt, d.Kind, token)
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
