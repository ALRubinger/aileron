package launch

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// prepareGitHubInject contract (#1149):
//
//   - A usable token yields GH_TOKEN (trimmed) plus a read-only file
//     mount at /home/agent/.gitconfig whose host source contains the
//     exact `!gh auth git-credential` helper line.
//   - A trailing newline in the stored secret is trimmed from GH_TOKEN.
//   - ErrUserCredentialsNotFound and a locked vault both yield an empty
//     prep with no error and a nil-safe Cleanup (skip-clean).
//   - An empty-after-trim token is treated as no token.
//   - The chown hook is invoked exactly once with the transient root
//     dir (P1 wiring), and the transient dir is 0700 (traverse bit),
//     never 0600.
//   - An unexpected daemon error aborts (it is not silently swallowed).

// fakeUserCredsDaemon is a one-method fake implementing userCredsDaemon.
type fakeUserCredsDaemon struct {
	secret vault.Secret
	err    error
	calls  int
}

func (f *fakeUserCredsDaemon) GetUserCredentials(_ context.Context, service string) (vault.Secret, error) {
	f.calls++
	if service != githubUserService {
		return vault.Secret{}, errors.New("unexpected service: " + service)
	}
	return f.secret, f.err
}

func TestPrepareGitHubInject_TokenPresent(t *testing.T) {
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("ghp_realtoken")}}
	prep, err := prepareGitHubInject(context.Background(), daemon, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()

	if got := prep.EnvAdditions["GH_TOKEN"]; got != "ghp_realtoken" {
		t.Errorf("GH_TOKEN = %q, want ghp_realtoken", got)
	}
	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1", len(prep.Mounts))
	}
	m := prep.Mounts[0]
	if m.Target != "/home/agent/.gitconfig" {
		t.Errorf("mount Target = %q, want /home/agent/.gitconfig", m.Target)
	}
	if !m.ReadOnly {
		t.Errorf("mount ReadOnly = false, want true")
	}
	content, readErr := os.ReadFile(m.Source)
	if readErr != nil {
		t.Fatalf("read mount source %q: %v", m.Source, readErr)
	}
	if !bytes.Contains(content, []byte("!gh auth git-credential")) {
		t.Errorf("gitconfig source missing helper line; got:\n%s", content)
	}
	if !bytes.Contains(content, []byte("[credential \"https://github.com\"]")) {
		t.Errorf("gitconfig source missing credential section; got:\n%s", content)
	}
}

func TestPrepareGitHubInject_TrimsTrailingNewline(t *testing.T) {
	// Regression: a secret stored with a trailing newline must not leak
	// the newline into GH_TOKEN (it would break gh's token parsing).
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("ghp_x\n")}}
	prep, err := prepareGitHubInject(context.Background(), daemon, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()
	if got := prep.EnvAdditions["GH_TOKEN"]; got != "ghp_x" {
		t.Errorf("GH_TOKEN = %q, want ghp_x (trimmed)", got)
	}
}

func TestPrepareGitHubInject_NotFoundIsCleanSkip(t *testing.T) {
	daemon := &fakeUserCredsDaemon{err: ErrUserCredentialsNotFound}
	prep, err := prepareGitHubInject(context.Background(), daemon, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	if _, ok := prep.EnvAdditions["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN set on not-found, want absent")
	}
	if len(prep.Mounts) != 0 {
		t.Errorf("Mounts = %d, want 0 on not-found", len(prep.Mounts))
	}
	if prep.Cleanup == nil {
		t.Fatal("Cleanup nil on not-found; want nil-safe no-op")
	}
	prep.Cleanup() // must not panic
}

func TestPrepareGitHubInject_LockedVaultIsCleanSkip(t *testing.T) {
	var stderr bytes.Buffer
	daemon := &fakeUserCredsDaemon{err: vault.ErrCredentialUnavailable}
	prep, err := prepareGitHubInject(context.Background(), daemon, nil, &stderr, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v, want clean skip on locked vault", err)
	}
	if _, ok := prep.EnvAdditions["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN set on locked vault, want absent")
	}
	if len(prep.Mounts) != 0 {
		t.Errorf("Mounts = %d, want 0 on locked vault", len(prep.Mounts))
	}
	if stderr.Len() == 0 {
		t.Errorf("expected a non-fatal locked-vault warning on stderr")
	}
	prep.Cleanup()
}

func TestPrepareGitHubInject_EmptyTokenIsCleanSkip(t *testing.T) {
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("   \n")}}
	prep, err := prepareGitHubInject(context.Background(), daemon, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	if _, ok := prep.EnvAdditions["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN set on empty-after-trim token, want absent")
	}
	if len(prep.Mounts) != 0 {
		t.Errorf("Mounts = %d, want 0 on empty token", len(prep.Mounts))
	}
	prep.Cleanup()
}

func TestPrepareGitHubInject_UnexpectedDaemonErrorAborts(t *testing.T) {
	daemon := &fakeUserCredsDaemon{err: errors.New("daemon down")}
	_, err := prepareGitHubInject(context.Background(), daemon, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error on unexpected daemon failure")
	}
	if errors.Is(err, ErrUserCredentialsNotFound) {
		t.Errorf("unexpected error collapsed to not-found: %v", err)
	}
}

func TestPrepareGitHubInject_ChownHookInvokedOnceWithTransientDir(t *testing.T) {
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("ghp_x")}}

	var seenDir string
	calls := 0
	hook := func(dir string) error {
		calls++
		seenDir = dir
		return nil
	}

	prep, err := prepareGitHubInject(context.Background(), daemon, nil, nil, hook)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()

	if calls != 1 {
		t.Errorf("chown hook called %d times, want exactly 1", calls)
	}
	// The hook must receive the transient ROOT dir (parent of the
	// rendered file), so the recursive chown covers both dir and file.
	if seenDir == "" {
		t.Fatal("chown hook received empty dir")
	}
	wantParent := seenDir + string(os.PathSeparator) + ".gitconfig"
	if prep.Mounts[0].Source != wantParent {
		t.Errorf("mount source %q is not under chowned dir %q", prep.Mounts[0].Source, seenDir)
	}

	info, statErr := os.Stat(seenDir)
	if statErr != nil {
		t.Fatalf("stat transient dir: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != fs.FileMode(0o700) {
		t.Errorf("transient dir perm = %o, want 0700 (needs traverse bit, not 0600)", perm)
	}
}

func TestPrepareGitHubInject_ChownHookFailureIsNonFatal(t *testing.T) {
	var stderr bytes.Buffer
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("ghp_x")}}
	hook := func(string) error { return errors.New("chown boom") }

	prep, err := prepareGitHubInject(context.Background(), daemon, nil, &stderr, hook)
	if err != nil {
		t.Fatalf("chown failure must be non-fatal, got: %v", err)
	}
	defer prep.Cleanup()
	if prep.EnvAdditions["GH_TOKEN"] != "ghp_x" {
		t.Errorf("GH_TOKEN not set after non-fatal chown failure")
	}
	if len(prep.Mounts) != 1 {
		t.Errorf("Mounts = %d, want 1 after non-fatal chown failure", len(prep.Mounts))
	}
	if stderr.Len() == 0 {
		t.Errorf("expected a non-fatal chown warning on stderr")
	}
}
