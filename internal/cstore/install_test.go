package cstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeFetcher serves bytes from an in-memory map keyed by URL. It captures
// every URL it was called with, so tests can assert resolution flowed
// through the resolver they expected.
type fakeFetcher struct {
	mu      sync.Mutex
	bytesAt map[string][]byte
	calls   []string
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, url)
	body, ok := f.bytesAt[url]
	if !ok {
		return nil, newError(ClassFetchFailed, BoundaryExternal, false, "no fixture for %s", url)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// happyPathInstaller wires an Installer with an in-memory fetcher whose
// canonical tarball lives at the resolver-computed URL for ref. Returns
// the installer, the canonical hash, and the in-memory fixture so tests
// can mutate as needed.
func happyPathInstaller(t *testing.T, ref Ref) (*Installer, string, *fakeFetcher) {
	t.Helper()
	manifest := []byte(canonicalManifestFor(ref.FQN.String(), ref.Version))
	binary := []byte("BINARY")
	sig, pub := signedManifest(t, binary, manifest)
	tarBytes := buildTarball(t, tarBinaryWASM, binary, manifest, sig)

	resolver := DefaultResolver()
	url, err := resolver.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	fetcher := &fakeFetcher{bytesAt: map[string][]byte{url: tarBytes}}

	keyring := NewEd25519Keyring()
	keyring.Add(ref.FQN.Authority(), pub)

	store := NewStore(t.TempDir())
	inst := &Installer{
		Resolver: resolver,
		Fetcher:  fetcher,
		Verifier: keyring,
		Store:    store,
	}
	tb := &Tarball{Binary: binary, Manifest: manifest}
	return inst, "sha256:" + tb.CanonicalHashHex(), fetcher
}

// TestInstall_ScopeDriftHookFiresWithManifestScopes: after a
// successful first install, the hook receives the connector's FQN
// and OAuth scope set. This is the seam #726 layers binding-drift
// detection on top of without dragging the binding package into
// cstore.
func TestInstall_ScopeDriftHookFiresWithManifestScopes(t *testing.T) {
	ref := mustRef(t, "github://acme/g@1.0.0")
	// Override the canonical manifest with one that declares OAuth2 scopes.
	manifest := []byte(`[connector]
name = "github://acme/g"
version = "1.0.0"
publisher = "test"

[capabilities.credential]
kind = "oauth2"
scope = "Read"

[capabilities.credential.oauth2]
authorize_url = "https://provider.test/auth"
token_url = "https://provider.test/token"
client_id = "cid"
scopes = ["read", "write"]
`)
	binary := []byte("BINARY")
	sig, pub := signedManifest(t, binary, manifest)
	tarBytes := buildTarball(t, tarBinaryWASM, binary, manifest, sig)
	resolver := DefaultResolver()
	url, err := resolver.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	fetcher := &fakeFetcher{bytesAt: map[string][]byte{url: tarBytes}}
	keyring := NewEd25519Keyring()
	keyring.Add(ref.FQN.Authority(), pub)

	var hookFQN string
	var hookScopes []string
	inst := &Installer{
		Resolver: resolver,
		Fetcher:  fetcher,
		Verifier: keyring,
		Store:    NewStore(t.TempDir()),
		ScopeDriftHook: func(_ context.Context, fqn string, required []string) []StaleBindingTransition {
			hookFQN = fqn
			hookScopes = required
			return nil
		},
	}
	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if hookFQN != "github://acme/g" {
		t.Errorf("hook FQN = %q", hookFQN)
	}
	if len(hookScopes) != 2 || hookScopes[0] != "read" || hookScopes[1] != "write" {
		t.Errorf("hook scopes = %v, want [read write]", hookScopes)
	}
}

// TestInstall_ScopeDriftHookFiresOnAlreadyInstalled: the short-circuit
// "this hash is already in the store" path must still fire the hook,
// otherwise the migration case (binding has no recorded grant) can
// never run if the user never installs a fresh hash.
func TestInstall_ScopeDriftHookFiresOnAlreadyInstalled(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, hash, _ := happyPathInstaller(t, ref)
	// First install populates the store.
	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	hookCalls := 0
	inst.ScopeDriftHook = func(_ context.Context, fqn string, _ []string) []StaleBindingTransition {
		hookCalls++
		if fqn != "github://aileron/slack" {
			t.Errorf("hook fqn = %q on short-circuit", fqn)
		}
		return nil
	}
	// Second install with the recorded ExpectedHash hits the
	// already-installed short-circuit.
	if _, err := inst.Install(context.Background(),
		InstallRequest{Ref: ref, ExpectedHash: hash}); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if hookCalls != 1 {
		t.Errorf("hook fired %d times on short-circuit, want 1", hookCalls)
	}
}

// TestInstall_ScopeDriftHookHandlesUnreadableManifest: the
// short-circuit path reads the installed manifest from disk to
// extract the scope set. A corrupt or unreadable manifest must not
// block the install — the hook fires with a nil scope list so the
// migration-mark case (binding has no recorded grant) can still run.
func TestInstall_ScopeDriftHookHandlesUnreadableManifest(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, hash, _ := happyPathInstaller(t, ref)
	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	// Corrupt the installed manifest so the hook's read-and-parse fails.
	dir, _ := inst.Store.EntryDir(hash)
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"),
		[]byte("not valid toml ====="), 0o644); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}
	var hookFired bool
	var hookScopes []string
	inst.ScopeDriftHook = func(_ context.Context, _ string, required []string) []StaleBindingTransition {
		hookFired = true
		hookScopes = required
		return nil
	}
	if _, err := inst.Install(context.Background(),
		InstallRequest{Ref: ref, ExpectedHash: hash}); err != nil {
		t.Fatalf("short-circuit Install: %v", err)
	}
	if !hookFired {
		t.Error("hook didn't fire when manifest was corrupt")
	}
	if hookScopes != nil {
		t.Errorf("hook got non-nil scopes from corrupt manifest: %v", hookScopes)
	}
}

// TestInstall_NewlyStaleTransitionsSurfacedOnFreshInstall: the
// installer surfaces the hook's stale-binding transitions on
// InstallResult.NewlyStale so the handler can include them in the
// API response (#741). Without this plumbing the install reports
// success and the next action invocation needing the new scope
// fails 403 against the upstream provider with no signal at install
// time. Covers the fresh-install path (steps through Verify+Commit).
func TestInstall_NewlyStaleTransitionsSurfacedOnFreshInstall(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, _, _ := happyPathInstaller(t, ref)
	stale := []StaleBindingTransition{
		{Name: "oauth2/slack/work", ConnectorFQN: ref.FQN.String(),
			StaleReason: "scope_drift", MissingScopes: []string{"chat:write"}},
	}
	inst.ScopeDriftHook = func(_ context.Context, _ string, _ []string) []StaleBindingTransition {
		return stale
	}
	res, err := inst.Install(context.Background(), InstallRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(res.NewlyStale) != 1 || res.NewlyStale[0].Name != "oauth2/slack/work" {
		t.Fatalf("NewlyStale = %+v, want one transition for oauth2/slack/work", res.NewlyStale)
	}
	if res.NewlyStale[0].StaleReason != "scope_drift" {
		t.Errorf("StaleReason = %q, want scope_drift", res.NewlyStale[0].StaleReason)
	}
}

// TestInstall_NewlyStaleTransitionsSurfacedOnShortCircuit: the
// already-installed short-circuit must propagate transitions too —
// that path is the migration case (binding has no recorded grant)
// the user described in #741. Without this branch a reinstall after
// upgrading the daemon (which adds scope tracking) would mark
// bindings stale but never tell the caller.
func TestInstall_NewlyStaleTransitionsSurfacedOnShortCircuit(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, hash, _ := happyPathInstaller(t, ref)
	// First install populates the store but doesn't return transitions.
	inst.ScopeDriftHook = func(_ context.Context, _ string, _ []string) []StaleBindingTransition {
		return nil
	}
	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	// Second install hits the short-circuit; arrange the hook to emit
	// a transition this time (mimics the migration mark).
	inst.ScopeDriftHook = func(_ context.Context, _ string, _ []string) []StaleBindingTransition {
		return []StaleBindingTransition{
			{Name: "oauth2/slack/work", ConnectorFQN: ref.FQN.String(),
				StaleReason: "no_grant_record"},
		}
	}
	res, err := inst.Install(context.Background(),
		InstallRequest{Ref: ref, ExpectedHash: hash})
	if err != nil {
		t.Fatalf("short-circuit Install: %v", err)
	}
	if !res.AlreadyInstalled {
		t.Fatal("expected short-circuit (AlreadyInstalled=true)")
	}
	if len(res.NewlyStale) != 1 || res.NewlyStale[0].StaleReason != "no_grant_record" {
		t.Errorf("NewlyStale = %+v, want one no_grant_record transition", res.NewlyStale)
	}
}

// TestInstall_ScopeDriftHookNilSafe: not all daemons wire a hook;
// installer must tolerate the unset field.
func TestInstall_ScopeDriftHookNilSafe(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, _, _ := happyPathInstaller(t, ref)
	inst.ScopeDriftHook = nil
	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Errorf("Install with nil hook should not error: %v", err)
	}
}

func TestInstall_HappyPath_RecordsHashAndStoresFiles(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, expectedHash, fetcher := happyPathInstaller(t, ref)

	res, err := inst.Install(context.Background(), InstallRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.AlreadyInstalled {
		t.Errorf("AlreadyInstalled = true on first install")
	}
	if res.Hash != expectedHash {
		t.Errorf("Hash = %q, want %q", res.Hash, expectedHash)
	}
	if !filepath.IsAbs(res.EntryDir) || filepath.Base(filepath.Dir(res.EntryDir)) != "sha256" {
		t.Errorf("EntryDir = %q; want absolute path under sha256/", res.EntryDir)
	}
	for _, name := range []string{tarBinaryWASM, tarManifestFile, tarSignatureFile} {
		if _, err := os.Stat(filepath.Join(res.EntryDir, name)); err != nil {
			t.Errorf("missing %s in entry: %v", name, err)
		}
	}
	if got, ok := inst.Store.Lookup(ref); !ok || got != expectedHash {
		t.Errorf("Lookup after install = (%q, %v)", got, ok)
	}

	// Sanity check: fetcher was actually called with the resolver-computed URL.
	want := "https://github.com/aileron/slack/releases/download/v1.2.0/aileron.tar.gz"
	if len(fetcher.calls) != 1 || fetcher.calls[0] != want {
		t.Errorf("fetcher calls = %v; want %q", fetcher.calls, want)
	}
}

func TestInstall_HashMismatch_LeavesStoreUntouched(t *testing.T) {
	// ADR-0004's failure-modes table:
	//   "Hash mismatch (action declares X, source serves Y) → Hard fail.
	//    The install was aborted; nothing was written to the store."
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, _, _ := happyPathInstaller(t, ref)

	_, err := inst.Install(context.Background(), InstallRequest{
		Ref:          ref,
		ExpectedHash: "sha256:0000000000",
	})
	if err == nil {
		t.Fatal("Install with hash mismatch succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassHashMismatch {
		t.Fatalf("err class = %v, want ClassHashMismatch", aerr)
	}

	// Store remains untouched.
	if entries, _ := os.ReadDir(inst.Store.sha256Root()); len(entries) != 0 {
		t.Errorf("sha256/ has %d entries; want 0 after hash-mismatch hard fail", len(entries))
	}
}

func TestInstall_SignatureFailure_LeavesStoreUntouched(t *testing.T) {
	// ADR-0004's failure-modes table:
	//   "Signature verification fails → Hard fail. No 'warning, you are
	//    about to install an unsigned binary' downgrade."
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, _, _ := happyPathInstaller(t, ref)
	// Replace the verifier with a strict keyring that has no key for the
	// authority — must fail closed per ADR-0004.
	inst.Verifier = NewEd25519Keyring()

	_, err := inst.Install(context.Background(), InstallRequest{Ref: ref})
	if err == nil {
		t.Fatal("Install with no key registered succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassSignatureFailure {
		t.Fatalf("err class = %v, want ClassSignatureFailure", aerr)
	}
	if entries, _ := os.ReadDir(inst.Store.sha256Root()); len(entries) != 0 {
		t.Errorf("sha256/ has %d entries; want 0 after signature-failure hard fail", len(entries))
	}
}

func TestInstall_FQNMismatchInManifest_HardFails(t *testing.T) {
	// ADR-0002: the manifest's `name` field "must match the FQN under
	// which the binary was published. Install tooling verifies this — a
	// binary fetched from `github://acme/gmail` whose manifest declares
	// `name = "github://other/gmail"` is rejected."
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	manifest := []byte(canonicalManifestFor("github://impostor/slack", "1.2.0"))
	binary := []byte("BIN")
	sig, pub := signedManifest(t, binary, manifest)
	tarBytes := buildTarball(t, tarBinaryWASM, binary, manifest, sig)

	resolver := DefaultResolver()
	url, _ := resolver.ResolveTarball(ref)
	keyring := NewEd25519Keyring()
	keyring.Add(ref.FQN.Authority(), pub)

	inst := &Installer{
		Resolver: resolver,
		Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: tarBytes}},
		Verifier: keyring,
		Store:    NewStore(t.TempDir()),
	}
	_, err := inst.Install(context.Background(), InstallRequest{Ref: ref})
	if err == nil {
		t.Fatal("Install with FQN mismatch succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassFQNMismatch {
		t.Errorf("err class = %v, want ClassFQNMismatch", aerr)
	}
}

func TestInstall_ExpectedHashAlreadyPresent_SkipsFetchAndReturnsAlreadyInstalled(t *testing.T) {
	// ADR-0004 §"Offline behavior":
	//   "Reinstall of an already-stored hash is offline. If (FQN, version,
	//    hash) resolves to an entry already in the store, the install
	//    pipeline short-circuits at step 7 — no download, no signature
	//    check on the already-trusted bytes."
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, expectedHash, fetcher := happyPathInstaller(t, ref)

	// First install populates the store.
	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	fetcher.calls = nil

	// Second install with the expected hash should skip the fetch.
	res, err := inst.Install(context.Background(), InstallRequest{
		Ref:          ref,
		ExpectedHash: expectedHash,
	})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !res.AlreadyInstalled {
		t.Errorf("AlreadyInstalled = false; want true on offline reinstall")
	}
	if len(fetcher.calls) != 0 {
		t.Errorf("fetcher was called %d times; want 0 on offline reinstall (calls=%v)", len(fetcher.calls), fetcher.calls)
	}
}

func TestInstall_RecordsHashWhenNoExpectedHash(t *testing.T) {
	// First-install path — caller doesn't yet know what hash to expect,
	// pipeline records whatever the bytes produce.
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, expectedHash, _ := happyPathInstaller(t, ref)
	res, err := inst.Install(context.Background(), InstallRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Hash != expectedHash {
		t.Errorf("Hash = %q, want %q", res.Hash, expectedHash)
	}
}

func TestInstall_FetchFailure_LeavesStoreUntouched(t *testing.T) {
	// Source-unreachable case: ADR-0004's failure-modes table demands hard
	// fail with no fallback.
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst := &Installer{
		Resolver: DefaultResolver(),
		Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{}},
		Verifier: PermissiveVerifier{},
		Store:    NewStore(t.TempDir()),
	}
	_, err := inst.Install(context.Background(), InstallRequest{Ref: ref})
	if err == nil {
		t.Fatal("Install with no fixture succeeded; want fetch failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassFetchFailed {
		t.Errorf("err class = %v, want ClassFetchFailed", aerr)
	}
	entries, _ := os.ReadDir(inst.Store.sha256Root())
	if len(entries) != 0 {
		t.Errorf("sha256/ has %d entries after fetch failure; want 0", len(entries))
	}
}

func TestInstall_RejectsNilDependencies(t *testing.T) {
	// Programmer error, not a runtime spec, but it ensures we don't
	// silently dereference nils. Each missing dep produces a non-nil
	// error explicitly so callers get a fast signal in tests.
	cases := []struct {
		name string
		mut  func(*Installer)
	}{
		{"no resolver", func(i *Installer) { i.Resolver = nil }},
		{"no fetcher", func(i *Installer) { i.Fetcher = nil }},
		{"no verifier", func(i *Installer) { i.Verifier = nil }},
		{"no store", func(i *Installer) { i.Store = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := &Installer{
				Resolver: DefaultResolver(),
				Fetcher:  &fakeFetcher{},
				Verifier: PermissiveVerifier{},
				Store:    NewStore(t.TempDir()),
			}
			tc.mut(i)
			_, err := i.Install(context.Background(), InstallRequest{Ref: mustRef(t, "github://x/y@1.0.0")})
			if err == nil {
				t.Errorf("Install with %s succeeded; want error", tc.name)
			}
		})
	}
}
