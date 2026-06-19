package launch

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/sentinel"
	"github.com/ALRubinger/aileron/internal/vault"
)

// prepareGitHubInject contract (#1149, sealed by #1195, sentinel-swap
// added by #1196):
//
//   - The agent never holds the GitHub secret. A usable user/github
//     entry yields a GH_TOKEN env set to the non-secret
//     sentinel.GitHubTokenSentinel (so `gh` does not short-circuit and
//     issues its request, which the daemon seals at the TLS boundary)
//     and a read-only file mount at /home/agent/.gitconfig whose host
//     source contains NO secret bytes, NO `!gh auth git-credential`
//     helper, and a no-op empty-credential helper scoped to
//     https://github.com. The real token is sealed daemon-side at egress,
//     never env-injected and never in the mount.
//   - Key Decision 4 (#1195): the static secret-free no-op gitconfig is
//     mounted on EVERY path — ErrUserCredentialsNotFound, a locked vault,
//     and an empty-after-trim token all yield exactly one read-only mount
//     at /home/agent/.gitconfig with no error and a real Cleanup that
//     removes the transient dir. The GH_TOKEN sentinel is gated to the
//     token-present path only (the sentinel-swap needs a real daemon-side
//     credential), so it is absent on those skip paths.
//   - An empty-after-trim token is treated as no token (mount, no token).
//   - The chown hook is invoked exactly once with the transient root
//     dir (P1 wiring), and the transient dir is 0700 (traverse bit),
//     never 0600.
//   - An unexpected daemon error aborts (it is not silently swallowed).

// githubMechBBindings returns the GitHub mechanism-B binding set the
// launcher passes to the planter. It is the canonical GitHub binding
// carrying the GH_TOKEN sentinel, so a planter test exercises the
// byte-for-byte GitHub plant path (#1247).
func githubMechBBindings(t *testing.T) []binding.HostBinding {
	t.Helper()
	hb, err := binding.NewHostBinding("api.github.com", "user/github", binding.SchemeBearer,
		binding.WithEmitMechanismB(), binding.WithSentinel(sentinel.GitHubTokenSentinel, "GH_TOKEN"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	return []binding.HostBinding{hb}
}

// fakeMultiServiceDaemon serves per-service secrets so a planter test can
// exercise a second, distinct mechanism-B binding alongside GitHub. A
// service with no entry returns ErrUserCredentialsNotFound.
type fakeMultiServiceDaemon struct {
	secrets map[string]vault.Secret
}

func (f *fakeMultiServiceDaemon) GetUserCredentials(_ context.Context, service string) (vault.Secret, error) {
	s, ok := f.secrets[service]
	if !ok {
		return vault.Secret{}, ErrUserCredentialsNotFound
	}
	return s, nil
}

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

func TestPrepareGitHubInject_SecondMechanismBBindingPlantedIndependently(t *testing.T) {
	// A second, distinct mechanism-B binding with its own value+env is
	// planted into its own env var, independently of GitHub (#1247), when
	// its credential resolves to a usable token.
	const otherSentinel = "sk_AILERONSENTINELOTHER"
	other, err := binding.NewHostBinding("api.other.test", "user/other", binding.SchemeBearer,
		binding.WithEmitMechanismB(), binding.WithSentinel(otherSentinel, "OTHER_TOKEN"))
	if err != nil {
		t.Fatalf("NewHostBinding(other): %v", err)
	}
	gh := githubMechBBindings(t)[0]

	daemon := &fakeMultiServiceDaemon{secrets: map[string]vault.Secret{
		"github": {Value: []byte("ghp_real")},
		"other":  {Value: []byte("sk_realother")},
	}}
	prep, err := prepareGitHubInject(context.Background(), daemon, []binding.HostBinding{gh, other}, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()

	if got := prep.EnvAdditions["GH_TOKEN"]; got != sentinel.GitHubTokenSentinel {
		t.Errorf("GH_TOKEN = %q, want the GitHub sentinel %q", got, sentinel.GitHubTokenSentinel)
	}
	if got := prep.EnvAdditions["OTHER_TOKEN"]; got != otherSentinel {
		t.Errorf("OTHER_TOKEN = %q, want the second binding's sentinel %q", got, otherSentinel)
	}
	// Neither real token bytes appear in the env.
	for k, v := range prep.EnvAdditions {
		if v == "ghp_real" || v == "sk_realother" {
			t.Errorf("EnvAdditions[%q] holds a real token; the agent must never see the secret", k)
		}
	}
}

func TestPrepareGitHubInject_SecondBindingWithoutCredentialPlantsNothing(t *testing.T) {
	// A mechanism-B binding whose credential ref has no usable vault entry
	// plants nothing (the swap would have nothing to swap to), while the
	// GitHub binding with a usable token still plants its sentinel.
	other, err := binding.NewHostBinding("api.other.test", "user/other", binding.SchemeBearer,
		binding.WithEmitMechanismB(), binding.WithSentinel("sk_AILERONSENTINELOTHER", "OTHER_TOKEN"))
	if err != nil {
		t.Fatalf("NewHostBinding(other): %v", err)
	}
	gh := githubMechBBindings(t)[0]

	// Only github resolves; "other" has no entry.
	daemon := &fakeMultiServiceDaemon{secrets: map[string]vault.Secret{
		"github": {Value: []byte("ghp_real")},
	}}
	prep, err := prepareGitHubInject(context.Background(), daemon, []binding.HostBinding{gh, other}, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()

	if _, ok := prep.EnvAdditions["OTHER_TOKEN"]; ok {
		t.Errorf("OTHER_TOKEN planted with no usable credential; want absent")
	}
	if got := prep.EnvAdditions["GH_TOKEN"]; got != sentinel.GitHubTokenSentinel {
		t.Errorf("GH_TOKEN = %q, want the GitHub sentinel (github still resolves)", got)
	}
}

func TestPrepareGitHubInject_SkipsNonBAndSentinellessBindings(t *testing.T) {
	// plantSentinels defensively skips a non-mechanism-B binding and a B
	// binding with no sentinel shape (the constructor normally rejects the
	// latter, so this is the fail-safe path). Neither plants any env.
	mechA, err := binding.NewHostBinding("api.a.test", "user/a", binding.SchemeBearer)
	if err != nil {
		t.Fatalf("NewHostBinding(mechA): %v", err)
	}
	sentinelless := binding.HostBinding{
		HostPattern:   "api.b.test",
		CredentialRef: "user/b",
		Scheme:        binding.SchemeBearer,
		EmitMechanism: binding.EmitMechanismB,
		// No SentinelValue/SentinelEnv: the defensive skip path.
	}

	daemon := &fakeMultiServiceDaemon{secrets: map[string]vault.Secret{
		"github": {Value: []byte("ghp_real")},
		"a":      {Value: []byte("a_real")},
		"b":      {Value: []byte("b_real")},
	}}
	prep, err := prepareGitHubInject(context.Background(), daemon,
		[]binding.HostBinding{githubMechBBindings(t)[0], mechA, sentinelless}, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()

	// Only the GitHub B binding with a sentinel plants anything.
	if got := prep.EnvAdditions["GH_TOKEN"]; got != sentinel.GitHubTokenSentinel {
		t.Errorf("GH_TOKEN = %q, want the GitHub sentinel", got)
	}
	if len(prep.EnvAdditions) != 1 {
		t.Errorf("EnvAdditions = %v, want only GH_TOKEN (non-B and sentinelless bindings plant nothing)", prep.EnvAdditions)
	}
}

func TestPrepareGitHubInject_TokenPresent(t *testing.T) {
	// Sealed model (#1195/#1196): a usable token mounts a secret-free
	// gitconfig and sets GH_TOKEN to the NON-SECRET sentinel (so `gh`
	// does not short-circuit). The real token bytes must never appear in
	// the env or the mounted file; the daemon swaps the sentinel for the
	// real credential at egress.
	const tokenValue = "ghp_realtoken"
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte(tokenValue)}}
	prep, err := prepareGitHubInject(context.Background(), daemon, githubMechBBindings(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()

	// GH_TOKEN must be present and equal to the sentinel, and recognized
	// by the proxy-side recognizer. This is the emit-mechanism B plant.
	ghToken, ok := prep.EnvAdditions["GH_TOKEN"]
	if !ok {
		t.Fatalf("GH_TOKEN absent from EnvAdditions; emit-mechanism B must plant the sentinel so gh does not short-circuit")
	}
	if ghToken != sentinel.GitHubTokenSentinel {
		t.Errorf("GH_TOKEN = %q, want the GitHub sentinel %q; the binding's SentinelValue is the single source of truth the proxy recognizer also reads", ghToken, sentinel.GitHubTokenSentinel)
	}
	// The real token bytes must never appear anywhere in the env. This is
	// the ADR-0019 regression guard: before this fix the real token was
	// injected here verbatim.
	for k, v := range prep.EnvAdditions {
		if v == tokenValue {
			t.Errorf("EnvAdditions[%q] holds the real token; the agent must never see the secret", k)
		}
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
	// No secret bytes in the mounted gitconfig.
	if bytes.Contains(content, []byte(tokenValue)) {
		t.Errorf("gitconfig source leaked the token; got:\n%s", content)
	}
	// No `gh` credential helper: emit-mechanism A is git-only, sealed at
	// egress; the agent must not shell out to gh for credentials.
	if bytes.Contains(content, []byte("!gh auth git-credential")) {
		t.Errorf("gitconfig source still references gh credential helper; got:\n%s", content)
	}
	// The no-op helper is scoped to the GitHub HTTPS credential section
	// so git emits a completable unauthenticated request instead of
	// blocking on a prompt.
	if !bytes.Contains(content, []byte("[credential \"https://github.com\"]")) {
		t.Errorf("gitconfig source missing credential section; got:\n%s", content)
	}
	if !bytes.Contains(content, []byte("helper =")) {
		t.Errorf("gitconfig source missing no-op (empty) helper; got:\n%s", content)
	}
}

func TestPrepareGitHubInject_GitconfigIsSecretFreeRegardlessOfToken(t *testing.T) {
	// Regression: whatever the stored token value, the mounted gitconfig
	// is byte-for-byte identical and contains no secret. This locks the
	// sealed state so a future change cannot reintroduce a token-bearing
	// gitconfig without breaking a test.
	a := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("ghp_alpha")}}
	b := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("ghp_bravo\n")}}

	prepA, err := prepareGitHubInject(context.Background(), a, githubMechBBindings(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject(a): %v", err)
	}
	defer prepA.Cleanup()
	prepB, err := prepareGitHubInject(context.Background(), b, githubMechBBindings(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject(b): %v", err)
	}
	defer prepB.Cleanup()

	contentA, _ := os.ReadFile(prepA.Mounts[0].Source)
	contentB, _ := os.ReadFile(prepB.Mounts[0].Source)
	if !bytes.Equal(contentA, contentB) {
		t.Errorf("gitconfig differs by token value; must be token-independent:\nA=%s\nB=%s", contentA, contentB)
	}
	for _, leak := range []string{"ghp_alpha", "ghp_bravo"} {
		if bytes.Contains(contentA, []byte(leak)) || bytes.Contains(contentB, []byte(leak)) {
			t.Errorf("gitconfig leaked token %q", leak)
		}
	}
}

func TestPrepareGitHubInject_NotFoundIsCleanSkip(t *testing.T) {
	// Key Decision 4 (#1195): a missing entry still mounts the static
	// secret-free no-op gitconfig (so git-over-HTTPS does not block), but
	// sets no GH_TOKEN (the sentinel-swap needs a real daemon-side
	// credential).
	daemon := &fakeUserCredsDaemon{err: ErrUserCredentialsNotFound}
	prep, err := prepareGitHubInject(context.Background(), daemon, githubMechBBindings(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	if _, ok := prep.EnvAdditions["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN set on not-found, want absent")
	}
	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (no-op gitconfig) on not-found", len(prep.Mounts))
	}
	if prep.Mounts[0].Target != "/home/agent/.gitconfig" {
		t.Errorf("mount Target = %q, want /home/agent/.gitconfig", prep.Mounts[0].Target)
	}
	if !prep.Mounts[0].ReadOnly {
		t.Errorf("mount ReadOnly = false, want true")
	}
	if prep.Cleanup == nil {
		t.Fatal("Cleanup nil on not-found; want nil-safe no-op")
	}
	src := prep.Mounts[0].Source
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("mounted gitconfig %q not present before cleanup: %v", src, statErr)
	}
	prep.Cleanup() // must not panic
	if _, statErr := os.Stat(src); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Cleanup did not remove transient dir: stat err = %v, want ErrNotExist", statErr)
	}
}

func TestPrepareGitHubInject_LockedVaultIsCleanSkip(t *testing.T) {
	// Key Decision 4 (#1195): a locked vault still mounts the static
	// secret-free no-op gitconfig and still emits the locked-vault
	// warning, but sets no GH_TOKEN.
	var stderr bytes.Buffer
	daemon := &fakeUserCredsDaemon{err: vault.ErrCredentialUnavailable}
	prep, err := prepareGitHubInject(context.Background(), daemon, githubMechBBindings(t), nil, &stderr, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v, want clean skip on locked vault", err)
	}
	defer prep.Cleanup()
	if _, ok := prep.EnvAdditions["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN set on locked vault, want absent")
	}
	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (no-op gitconfig) on locked vault", len(prep.Mounts))
	}
	if prep.Mounts[0].Target != "/home/agent/.gitconfig" {
		t.Errorf("mount Target = %q, want /home/agent/.gitconfig", prep.Mounts[0].Target)
	}
	if !prep.Mounts[0].ReadOnly {
		t.Errorf("mount ReadOnly = false, want true")
	}
	if stderr.Len() == 0 {
		t.Errorf("expected a non-fatal locked-vault warning on stderr")
	}
}

func TestPrepareGitHubInject_EmptyTokenIsCleanSkip(t *testing.T) {
	// Key Decision 4 (#1195): an empty-after-trim token still mounts the
	// static secret-free no-op gitconfig, but sets no GH_TOKEN.
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("   \n")}}
	prep, err := prepareGitHubInject(context.Background(), daemon, githubMechBBindings(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer prep.Cleanup()
	if _, ok := prep.EnvAdditions["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN set on empty-after-trim token, want absent")
	}
	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (no-op gitconfig) on empty token", len(prep.Mounts))
	}
	if prep.Mounts[0].Target != "/home/agent/.gitconfig" {
		t.Errorf("mount Target = %q, want /home/agent/.gitconfig", prep.Mounts[0].Target)
	}
	if !prep.Mounts[0].ReadOnly {
		t.Errorf("mount ReadOnly = false, want true")
	}
}

func TestPrepareGitHubInject_UnexpectedDaemonErrorAborts(t *testing.T) {
	daemon := &fakeUserCredsDaemon{err: errors.New("daemon down")}
	_, err := prepareGitHubInject(context.Background(), daemon, githubMechBBindings(t), nil, nil, nil)
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

	prep, err := prepareGitHubInject(context.Background(), daemon, githubMechBBindings(t), nil, nil, hook)
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

	prep, err := prepareGitHubInject(context.Background(), daemon, githubMechBBindings(t), nil, &stderr, hook)
	if err != nil {
		t.Fatalf("chown failure must be non-fatal, got: %v", err)
	}
	defer prep.Cleanup()
	// A non-fatal chown failure still produces the full prep: the
	// non-secret sentinel GH_TOKEN and the secret-free mount. The real
	// token never appears regardless.
	if prep.EnvAdditions["GH_TOKEN"] != sentinel.GitHubTokenSentinel {
		t.Errorf("GH_TOKEN after chown failure = %q, want the sentinel", prep.EnvAdditions["GH_TOKEN"])
	}
	if prep.EnvAdditions["GH_TOKEN"] == "ghp_x" {
		t.Errorf("GH_TOKEN holds the real token after chown failure; the agent must never see the secret")
	}
	if len(prep.Mounts) != 1 {
		t.Errorf("Mounts = %d, want 1 after non-fatal chown failure", len(prep.Mounts))
	}
	if stderr.Len() == 0 {
		t.Errorf("expected a non-fatal chown warning on stderr")
	}
}
