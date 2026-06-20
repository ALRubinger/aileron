package capture

import (
	"context"
	"reflect"
	"testing"
)

// This test is the acceptance gate for #1287: it proves the
// descriptor-driven `gh` acquisition produces the byte-identical container
// exec arg vectors and the same store call (user/github, kind user) as the
// bespoke containerDeviceFlow in cmd/aileron/auth_github.go. It binds the
// shipped gh.yaml onto the REAL capture.Driver via the registry and runs
// the real Driver.Acquire path through the existing recordingRunner seam —
// no stub driver, no invented interface.
//
// The bespoke flow (auth_github.go) emits exactly:
//
//	login: exec -i [-t] --env=BROWSER=echo aileron-auth-github \
//	         gh auth login --hostname github.com --git-protocol https --web
//	token: exec aileron-auth-github gh auth token --hostname github.com
//
// with NO config-dir --env token on either exec. Parity hinges on gh.yaml
// omitting config_dir; if it were set, both vectors would gain an --env=
// token the bespoke flow never emits.

// bindGhFromRegistry resolves gh from the registry, builds a bare Driver
// (no runtime resolution needed — the recordingRunner is injected), applies
// the descriptor, and wires the recording runner + store. It exercises Apply
// against the real Driver type end-to-end.
//
// Post-#1323 the built-in capture layer is empty (gh moved to its
// devcontainer Feature CLI unit), so gh is sourced through the unit-derived
// layer (CaptureLoadOptions.ExtraDescriptors) using the in-package gh
// literal — the same path the host uses when it projects gh from the image's
// devcontainer.metadata. The capture package cannot import unitloader
// (import cycle), so the literal lives here; internal/app's drift guard pins
// it to the live Feature manifest.
func bindGhFromRegistry(t *testing.T, rr *recordingRunner, store StoreFunc) *Driver {
	t.Helper()
	r, err := NewRegistry(CaptureLoadOptions{
		ExtraDescriptors: []CaptureDescriptor{ghCaptureLiteral()},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, ok := r.Resolve("gh")
	if !ok {
		t.Fatal("registry has no gh descriptor")
	}
	drv := &Driver{
		Runner:     runnerFunc(rr.Run),
		RuntimeExe: "docker",
	}
	// Caller resolves the image; gh.yaml leaves image empty.
	d.Apply(drv, "ghcr.io/example/gh:latest", store)
	return drv
}

func TestGhParity_ExecArgVectorsAndStore_NonTTY(t *testing.T) {
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = orig })

	rr := &recordingRunner{token: "gho_paritytoken"}
	store := &recordingStore{}
	drv := bindGhFromRegistry(t, rr, store.fn())

	if err := drv.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	login, token := execArgsBySub(rr.calls)

	wantLogin := []string{
		"exec", "-i", "--env=BROWSER=echo", "aileron-auth-github",
		"gh", "auth", "login",
		"--hostname", "github.com",
		"--git-protocol", "https",
		"--web",
	}
	if !reflect.DeepEqual(login, wantLogin) {
		t.Errorf("login exec vector mismatch\n got: %v\nwant: %v", login, wantLogin)
	}

	wantToken := []string{
		"exec", "aileron-auth-github",
		"gh", "auth", "token", "--hostname", "github.com",
	}
	if !reflect.DeepEqual(token, wantToken) {
		t.Errorf("token exec vector mismatch\n got: %v\nwant: %v", token, wantToken)
	}

	if store.called != 1 {
		t.Fatalf("Store called %d times, want exactly 1", store.called)
	}
	if store.storeAt != "user/github" {
		t.Errorf("storeAt = %q, want user/github", store.storeAt)
	}
	if store.kind != "user" {
		t.Errorf("kind = %q, want user", store.kind)
	}
	if string(store.value) != "gho_paritytoken" {
		t.Errorf("stored token = %q, want the trimmed captured value", store.value)
	}
}

func TestGhParity_LoginGetsPTYWhenTTY(t *testing.T) {
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = orig })

	rr := &recordingRunner{token: "gho_tok"}
	drv := bindGhFromRegistry(t, rr, (&recordingStore{}).fn())
	if err := drv.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	login, token := execArgsBySub(rr.calls)
	wantLogin := []string{
		"exec", "-i", "-t", "--env=BROWSER=echo", "aileron-auth-github",
		"gh", "auth", "login",
		"--hostname", "github.com",
		"--git-protocol", "https",
		"--web",
	}
	if !reflect.DeepEqual(login, wantLogin) {
		t.Errorf("login exec vector mismatch (TTY)\n got: %v\nwant: %v", login, wantLogin)
	}
	// The token read never gets -t and never carries the BROWSER shim.
	for _, a := range token {
		if a == "-t" || a == "--env=BROWSER=echo" {
			t.Errorf("token exec must not carry %q: %v", a, token)
		}
	}
}

// TestGhParity_NoConfigDirEnvOnEitherExec is the explicit guard for the
// parity-critical omission: gh.yaml must not set config_dir, so neither
// exec carries an --env=*CONFIG_DIR token. This is the assertion that
// catches a future edit that mistakenly adds config_dir.
func TestGhParity_NoConfigDirEnvOnEitherExec(t *testing.T) {
	rr := &recordingRunner{token: "gho_tok"}
	drv := bindGhFromRegistry(t, rr, (&recordingStore{}).fn())
	if err := drv.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	login, token := execArgsBySub(rr.calls)
	for _, vec := range [][]string{login, token} {
		for _, a := range vec {
			if len(a) >= 6 && a[:6] == "--env=" && a != "--env=BROWSER=echo" {
				t.Errorf("unexpected env token %q in exec vector %v (gh must omit config_dir)", a, vec)
			}
		}
	}
}
