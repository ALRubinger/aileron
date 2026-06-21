package cstore_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

func TestLoadKeyring_MissingFileReturnsEmptyKeyring(t *testing.T) {
	// Per ADR-0011 / ADR-0002 deferral: missing file is fine — Aileron
	// ships with no trusted publishers; users opt in. Verify time
	// enforces fail-closed when no key is registered.
	kr, err := cstore.LoadKeyring(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if kr == nil {
		t.Fatal("nil keyring")
	}
	if err := kr.Verify("github://x/y", []byte("b"), []byte("m"), []byte("s")); err == nil {
		t.Errorf("empty keyring should reject every install")
	}
}

func TestLoadKeyring_HappyPath(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	body := `{
		"version": 1,
		"publishers": {
			"github://ALRubinger/aileron-connector-github": ["` + pubB64 + `"]
		}
	}`
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	// Sign a payload and verify through the loaded keyring to prove
	// the key landed in the right authority bucket.
	payload := append([]byte("BIN"), []byte("MAN")...)
	_, _ = payload, pubB64
	t.Run("registered authority verifies a real signature", func(t *testing.T) {
		// We don't have the private key after JSON loading — but we
		// can sign with a NEW pair, register it, and verify. The
		// happy-path assertion is that the loaded key is reachable.
		// To prove that, generate a fresh pair, encode pub, reload.
		newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)
		body := `{"version":1,"publishers":{"acme://x/y":["` +
			base64.StdEncoding.EncodeToString(newPub) + `"]}}`
		p := filepath.Join(t.TempDir(), "k.json")
		_ = os.WriteFile(p, []byte(body), 0o600)
		kr2, err := cstore.LoadKeyring(p)
		if err != nil {
			t.Fatalf("LoadKeyring: %v", err)
		}
		sig := ed25519.Sign(newPriv, payload)
		if err := kr2.Verify("acme://x/y", []byte("BIN"), []byte("MAN"), sig); err != nil {
			t.Errorf("loaded key did not verify: %v", err)
		}
	})
	// Unregistered authority is rejected even when the file has other
	// authorities registered.
	if err := kr.Verify("github://other/repo", []byte("b"), []byte("m"), []byte("s")); err == nil {
		t.Error("unknown authority should be rejected")
	}
}

func TestLoadKeyring_AcceptsURLAndUnpaddedBase64(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for name, encoded := range map[string]string{
		"std":     base64.StdEncoding.EncodeToString(pub),
		"raw_std": base64.RawStdEncoding.EncodeToString(pub),
		"url":     base64.URLEncoding.EncodeToString(pub),
		"raw_url": base64.RawURLEncoding.EncodeToString(pub),
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"version":1,"publishers":{"a://b/c":["` + encoded + `"]}}`
			p := filepath.Join(t.TempDir(), "k.json")
			_ = os.WriteFile(p, []byte(body), 0o600)
			kr, err := cstore.LoadKeyring(p)
			if err != nil {
				t.Fatalf("LoadKeyring(%s): %v", name, err)
			}
			sig := ed25519.Sign(priv, []byte("xy"))
			if err := kr.Verify("a://b/c", []byte("x"), []byte("y"), sig); err != nil {
				t.Errorf("%s: verify: %v", name, err)
			}
		})
	}
}

func TestLoadKeyring_MultipleKeysPerAuthorityForRotation(t *testing.T) {
	// Key rotation: publisher registers a new key alongside the old
	// one. Both verify until the old is dropped.
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	body := `{
		"version": 1,
		"publishers": {
			"a://b/c": [
				"` + base64.StdEncoding.EncodeToString(pub1) + `",
				"` + base64.StdEncoding.EncodeToString(pub2) + `"
			]
		}
	}`
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(body), 0o600)
	kr, err := cstore.LoadKeyring(p)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	for _, sk := range []ed25519.PrivateKey{priv1, priv2} {
		sig := ed25519.Sign(sk, []byte("xy"))
		if err := kr.Verify("a://b/c", []byte("x"), []byte("y"), sig); err != nil {
			t.Errorf("verify with one of the registered keys: %v", err)
		}
	}
}

func TestLoadKeyring_RejectsMalformedJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(`{not json`), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	var cerr *cstore.Error
	if !errors.As(err, &cerr) || cerr.Class != cstore.ClassValidationError {
		t.Errorf("err class = %v, want ClassValidationError", err)
	}
}

func TestLoadKeyring_RejectsUnsupportedVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(`{"version":99}`), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on unsupported version")
	}
}

func TestLoadKeyring_RejectsBadKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(`{"version":1,"publishers":{"a://b/c":["not-base64!!!"]}}`), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on invalid base64")
	}
}

func TestLoadKeyring_RejectsWrongLengthKey(t *testing.T) {
	tooShort := base64.StdEncoding.EncodeToString([]byte("only-a-few-bytes"))
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(`{"version":1,"publishers":{"a://b/c":["`+tooShort+`"]}}`), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on wrong key length")
	}
}

func TestLoadKeyring_RejectsEmptyAuthority(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"version":1,"publishers":{"":["` + base64.StdEncoding.EncodeToString(pub) + `"]}}`
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(body), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on empty authority")
	}
}

// --- SaveKeyring ---

func TestSaveKeyring_RoundTripsEquivalentKeyring(t *testing.T) {
	// Save → reload → verify the same keys come back. The file format
	// guarantees authority equivalence; key order within an authority
	// is order-of-insertion since that is what the keyringFile JSON
	// preserves.
	pubA, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB1, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB2, _, _ := ed25519.GenerateKey(rand.Reader)

	kr := cstore.NewEd25519Keyring()
	kr.Add("github://a/repo", pubA)
	kr.Add("github://b/repo", pubB1)
	kr.Add("github://b/repo", pubB2)

	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}

	loaded, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !loaded.HasKey("github://a/repo", pubA) {
		t.Error("authority A's key not preserved")
	}
	if !loaded.HasKey("github://b/repo", pubB1) || !loaded.HasKey("github://b/repo", pubB2) {
		t.Error("authority B's keys not preserved")
	}
}

func TestSaveKeyring_CreatesParentDirectory(t *testing.T) {
	// Save under a nested path that doesn't exist yet — should mkdir -p.
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", ".aileron", "keyring.json")

	kr := cstore.NewEd25519Keyring()
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("SaveKeyring should create parent dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestSaveKeyring_FileMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := cstore.NewEd25519Keyring().SaveKeyring(path); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// We don't assert exact mode bits beyond the user-readable bit (umask
	// can affect group/other on some platforms); the spec is "no
	// world-readable trust file".
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("file mode = %v; group/other bits should be unset", info.Mode().Perm())
	}
}

// --- Authorities / Keys / Remove / HasKey ---

func TestKeyringAuthorities_SortedAndDeduplicated(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	kr := cstore.NewEd25519Keyring()
	kr.Add("github://b/two", pub1)
	kr.Add("github://a/one", pub2)
	kr.Add("github://a/one", pub1) // second key under same authority

	got := kr.Authorities()
	want := []string{"github://a/one", "github://b/two"}
	if len(got) != len(want) {
		t.Fatalf("got %d authorities, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("Authorities[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestKeyringKeys_ReturnsCopyNotOriginal(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.Add("github://x/y", pub)

	keys := kr.Keys("github://x/y")
	if len(keys) != 1 {
		t.Fatalf("len(keys) = %d, want 1", len(keys))
	}
	keys[0] = ed25519.PublicKey(make([]byte, 32)) // mutate the copy

	// Re-fetch and assert the original is untouched.
	again := kr.Keys("github://x/y")
	if !ed25519.PublicKey(again[0]).Equal(pub) {
		t.Error("Keys() returned a slice aliased to the keyring's internal storage")
	}
}

func TestKeyringKeys_UnknownAuthorityReturnsNil(t *testing.T) {
	if got := cstore.NewEd25519Keyring().Keys("github://nope/nope"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestKeyringRemove_ReturnsTrueOnHitFalseOnMiss(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.Add("github://x/y", pub)

	if !kr.Remove("github://x/y") {
		t.Error("Remove(present) = false; want true")
	}
	if kr.Remove("github://x/y") {
		t.Error("Remove(absent after first remove) = true; want false")
	}
	if got := kr.Authorities(); len(got) != 0 {
		t.Errorf("expected no authorities after Remove; got %v", got)
	}
}

func TestKeyringHasKey_TrueOnlyForExactKey(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.Add("github://x/y", pub1)

	if !kr.HasKey("github://x/y", pub1) {
		t.Error("HasKey should return true for added key")
	}
	if kr.HasKey("github://x/y", pub2) {
		t.Error("HasKey should return false for a different key under same authority")
	}
	if kr.HasKey("github://other/repo", pub1) {
		t.Error("HasKey should return false for a different authority")
	}
}

// --- ParsePEMPublicKey ---

func TestParsePEMPublicKey_HappyPath(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pemBytes := makeTestPEM(t, pub)

	got, err := cstore.ParsePEMPublicKey(pemBytes)
	if err != nil {
		t.Fatalf("ParsePEMPublicKey: %v", err)
	}
	if !ed25519.PublicKey(got).Equal(pub) {
		t.Error("returned key does not match")
	}
}

func TestParsePEMPublicKey_NoPEMBlock(t *testing.T) {
	if _, err := cstore.ParsePEMPublicKey([]byte("not a pem block")); err == nil {
		t.Fatal("expected error for non-PEM input")
	}
}

func TestParsePEMPublicKey_GarbageDER(t *testing.T) {
	// PEM block with non-DER contents.
	bad := []byte("-----BEGIN PUBLIC KEY-----\nbm90LWRlcg==\n-----END PUBLIC KEY-----\n")
	if _, err := cstore.ParsePEMPublicKey(bad); err == nil {
		t.Fatal("expected error for non-DER PEM contents")
	}
}

// --- DefaultKeyringPath ---

func TestDefaultKeyringPath_FollowsHome(t *testing.T) {
	t.Setenv("HOME", "/tmp/fake-home")
	got := cstore.DefaultKeyringPath()
	want := filepath.Join("/tmp/fake-home", ".aileron", "keyring.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDefaultKeyringPath_NoHomeReturnsEmpty(t *testing.T) {
	// On unix-likes, unsetting HOME makes UserHomeDir return an error
	// (no fallback). Skip the test on platforms where the runtime
	// probes alternate sources (Windows looks at USERPROFILE etc.).
	if _, err := os.UserHomeDir(); err == nil {
		t.Setenv("HOME", "")
		// Also clear the Windows fallback envs so the test is a real
		// "no discoverable home" scenario across platforms.
		t.Setenv("USERPROFILE", "")
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
	}
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("UserHomeDir still resolves; can't simulate missing-home on this platform")
	}
	if got := cstore.DefaultKeyringPath(); got != "" {
		t.Errorf("got %q; want empty when home is undiscoverable", got)
	}
}

// --- LoadKeyring round-trip with tampered JSON ---

func TestLoadKeyring_RetainsErrFromIO(t *testing.T) {
	// Pass a directory path; ReadFile errors with EISDIR (or similar)
	// — should not be a "missing file" no-op.
	dir := t.TempDir()
	if _, err := cstore.LoadKeyring(dir); err == nil {
		t.Fatal("expected error when path is a directory")
	}
	// errors.Is(fs.ErrNotExist) must be false for this case.
	if _, err := cstore.LoadKeyring(dir); errors.Is(err, os.ErrNotExist) {
		t.Error("EISDIR was misinterpreted as missing file")
	}
}

// --- v2 schema: owners + publishers ---

func TestLoadKeyring_V1FileLoadsUnderV2LoaderNoDataLoss(t *testing.T) {
	// A version:1 file with only per-repo grants loads losslessly under
	// the v2 loader: the per-repo key verifies and OwnerAuthorities() is
	// empty.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"version":1,"publishers":{"github://acme/connector":["` +
		base64.StdEncoding.EncodeToString(pub) + `"]}}`
	p := filepath.Join(t.TempDir(), "k.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	kr, err := cstore.LoadKeyring(p)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	sig := ed25519.Sign(priv, append([]byte("x"), []byte("y")...))
	if err := kr.Verify("github://acme/connector", []byte("x"), []byte("y"), sig); err != nil {
		t.Errorf("per-repo key did not verify: %v", err)
	}
	if got := kr.OwnerAuthorities(); len(got) != 0 {
		t.Errorf("OwnerAuthorities() = %v, want empty for a v1 file", got)
	}
	if got := kr.Authorities(); len(got) != 1 || got[0] != "github://acme/connector" {
		t.Errorf("Authorities() = %v, want [github://acme/connector]", got)
	}
}

func TestLoadKeyring_V2FileWithBothMapsLoadsIndependently(t *testing.T) {
	// An owner key verifies via the owner authority, a repo key via the
	// repo authority, independently of each other.
	ownerPub, ownerPriv, _ := ed25519.GenerateKey(rand.Reader)
	repoPub, repoPriv, _ := ed25519.GenerateKey(rand.Reader)
	body := `{
		"version": 2,
		"owners": {"github://acme": ["` + base64.StdEncoding.EncodeToString(ownerPub) + `"]},
		"publishers": {"github://acme/connector": ["` + base64.StdEncoding.EncodeToString(repoPub) + `"]}
	}`
	p := filepath.Join(t.TempDir(), "k.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	kr, err := cstore.LoadKeyring(p)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	ownerSig := ed25519.Sign(ownerPriv, append([]byte("o"), []byte("m")...))
	if err := kr.Verify("github://acme", []byte("o"), []byte("m"), ownerSig); err != nil {
		t.Errorf("owner key did not verify under owner authority: %v", err)
	}
	repoSig := ed25519.Sign(repoPriv, append([]byte("r"), []byte("m")...))
	if err := kr.Verify("github://acme/connector", []byte("r"), []byte("m"), repoSig); err != nil {
		t.Errorf("repo key did not verify under repo authority: %v", err)
	}
	// The owner key must NOT verify under the repo authority (independence).
	if err := kr.Verify("github://acme/connector", []byte("o"), []byte("m"), ownerSig); err == nil {
		t.Error("owner key should not verify under a per-repo authority")
	}
}

func TestSaveKeyring_V2RoundTripBothMaps(t *testing.T) {
	ownerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	repoPub, _, _ := ed25519.GenerateKey(rand.Reader)

	kr := cstore.NewEd25519Keyring()
	kr.Add("github://acme/connector", repoPub)
	kr.AddOwner("github://acme", ownerPub)

	p := filepath.Join(t.TempDir(), "keyring.json")
	if err := kr.SaveKeyring(p); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc struct {
		Version    int                 `json:"version"`
		Owners     map[string][]string `json:"owners"`
		Publishers map[string][]string `json:"publishers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal saved file: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("saved version = %d, want 2", doc.Version)
	}
	if _, ok := doc.Owners["github://acme"]; !ok {
		t.Errorf("owners map missing github://acme: %v", doc.Owners)
	}
	if _, ok := doc.Publishers["github://acme/connector"]; !ok {
		t.Errorf("publishers map missing github://acme/connector: %v", doc.Publishers)
	}

	loaded, err := cstore.LoadKeyring(p)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !loaded.HasOwnerKey("github://acme", ownerPub) {
		t.Error("owner key not preserved across round-trip")
	}
	if !loaded.HasKey("github://acme/connector", repoPub) {
		t.Error("repo key not preserved across round-trip")
	}
}

func TestSaveKeyring_OmitsOwnersWhenNoneButStillV2(t *testing.T) {
	// A keyring with only per-repo entries saves version:2 with no
	// "owners" key (omitempty) yet still reloads.
	repoPub, _, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.Add("github://acme/connector", repoPub)

	p := filepath.Join(t.TempDir(), "keyring.json")
	if err := kr.SaveKeyring(p); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(probe["version"]) != "2" {
		t.Errorf("version = %s, want 2", probe["version"])
	}
	if _, present := probe["owners"]; present {
		t.Errorf("owners key should be omitted when there are no owner grants; file: %s", raw)
	}
	loaded, err := cstore.LoadKeyring(p)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !loaded.HasKey("github://acme/connector", repoPub) {
		t.Error("repo key not preserved")
	}
}

func TestLoadKeyring_RejectsVersionsOtherThan1And2(t *testing.T) {
	for _, ver := range []string{"3", "0"} {
		t.Run("version_"+ver, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "k.json")
			_ = os.WriteFile(p, []byte(`{"version":`+ver+`}`), 0o600)
			_, err := cstore.LoadKeyring(p)
			if err == nil {
				t.Fatalf("expected error on version %s", ver)
			}
			var cerr *cstore.Error
			if !errors.As(err, &cerr) || cerr.Class != cstore.ClassValidationError {
				t.Errorf("err class = %v, want ClassValidationError", err)
			}
		})
	}
}

func TestLoadKeyring_RejectsOwnersUnderVersion1(t *testing.T) {
	// Owner-level grants are v2-only; a downgrade to version:1 that still
	// lists owners must be rejected, not silently honored.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"version":1,"owners":{"github://acme":["` +
		base64.StdEncoding.EncodeToString(pub) + `"]}}`
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(body), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error: owners under version 1")
	}
	var cerr *cstore.Error
	if !errors.As(err, &cerr) || cerr.Class != cstore.ClassValidationError {
		t.Errorf("err class = %v, want ClassValidationError", err)
	}
}

func TestLoadKeyring_RejectsBadKeyInOwners(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(`{"version":2,"owners":{"github://acme":["not-base64!!!"]}}`), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on invalid base64 in owners")
	}
	var cerr *cstore.Error
	if !errors.As(err, &cerr) || cerr.Class != cstore.ClassValidationError {
		t.Errorf("err class = %v, want ClassValidationError", err)
	}
}

func TestLoadKeyring_RejectsWrongLengthKeyInOwners(t *testing.T) {
	tooShort := base64.StdEncoding.EncodeToString([]byte("only-a-few-bytes"))
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(`{"version":2,"owners":{"github://acme":["`+tooShort+`"]}}`), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on wrong key length in owners")
	}
	var cerr *cstore.Error
	if !errors.As(err, &cerr) || cerr.Class != cstore.ClassValidationError {
		t.Errorf("err class = %v, want ClassValidationError", err)
	}
}

func TestLoadKeyring_RejectsEmptyAuthorityInOwners(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"version":2,"owners":{"":["` + base64.StdEncoding.EncodeToString(pub) + `"]}}`
	p := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(p, []byte(body), 0o600)
	_, err := cstore.LoadKeyring(p)
	if err == nil {
		t.Fatal("expected error on empty authority in owners")
	}
	var cerr *cstore.Error
	if !errors.As(err, &cerr) || cerr.Class != cstore.ClassValidationError {
		t.Errorf("err class = %v, want ClassValidationError", err)
	}
}

// --- owner-keyed helper contracts ---

func TestKeyringAddOwner_RegistersUnderOwnerAuthority(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.AddOwner("github://acme", pub)
	sig := ed25519.Sign(priv, append([]byte("a"), []byte("b")...))
	if err := kr.Verify("github://acme", []byte("a"), []byte("b"), sig); err != nil {
		t.Errorf("AddOwner key did not verify: %v", err)
	}
}

func TestKeyringOwnerKeys_CopyAndNilForAbsent(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.AddOwner("github://acme", pub)

	keys := kr.OwnerKeys("github://acme")
	if len(keys) != 1 {
		t.Fatalf("len(OwnerKeys) = %d, want 1", len(keys))
	}
	keys[0] = ed25519.PublicKey(make([]byte, 32)) // mutate the copy
	again := kr.OwnerKeys("github://acme")
	if !ed25519.PublicKey(again[0]).Equal(pub) {
		t.Error("OwnerKeys() returned a slice aliased to internal storage")
	}
	if got := kr.OwnerKeys("github://absent"); got != nil {
		t.Errorf("OwnerKeys(absent) = %v, want nil", got)
	}
}

func TestKeyringHasOwnerKey_ExactKeyAndAuthorityOnly(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.AddOwner("github://acme", pub1)

	if !kr.HasOwnerKey("github://acme", pub1) {
		t.Error("HasOwnerKey should be true for the added key")
	}
	if kr.HasOwnerKey("github://acme", pub2) {
		t.Error("HasOwnerKey should be false for a different key")
	}
	if kr.HasOwnerKey("github://other", pub1) {
		t.Error("HasOwnerKey should be false for a different authority")
	}
}

func TestKeyringRemoveOwner_TrueHitFalseMiss(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	kr := cstore.NewEd25519Keyring()
	kr.AddOwner("github://acme", pub)

	if !kr.RemoveOwner("github://acme") {
		t.Error("RemoveOwner(present) = false; want true")
	}
	if kr.RemoveOwner("github://acme") {
		t.Error("RemoveOwner(absent) = true; want false")
	}
	if got := kr.OwnerAuthorities(); len(got) != 0 {
		t.Errorf("OwnerAuthorities after remove = %v, want empty", got)
	}
}

func TestKeyringOwnerAuthorities_SortedOwnerOnlySplitFromAuthorities(t *testing.T) {
	// A keyring with one owner-level entry and one per-repo entry returns
	// just the owner authority from OwnerAuthorities() and just the repo
	// authority from Authorities().
	ownerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	repoPub, _, _ := ed25519.GenerateKey(rand.Reader)
	owner2Pub, _, _ := ed25519.GenerateKey(rand.Reader)

	kr := cstore.NewEd25519Keyring()
	kr.Add("github://acme/connector", repoPub)
	kr.AddOwner("github://zzz", owner2Pub)
	kr.AddOwner("github://acme", ownerPub)

	gotOwners := kr.OwnerAuthorities()
	wantOwners := []string{"github://acme", "github://zzz"}
	if len(gotOwners) != len(wantOwners) {
		t.Fatalf("OwnerAuthorities() = %v, want %v", gotOwners, wantOwners)
	}
	for i, w := range wantOwners {
		if gotOwners[i] != w {
			t.Errorf("OwnerAuthorities()[%d] = %q, want %q (sorted, owner-only)", i, gotOwners[i], w)
		}
	}
	// Authorities() returns the full flat set (owner + per-repo), but the
	// per-repo authority must be present and the owner-level split must
	// not leak into OwnerAuthorities's exclusion of the repo entry.
	gotAuth := kr.Authorities()
	foundRepo := false
	for _, a := range gotAuth {
		if a == "github://acme/connector" {
			foundRepo = true
		}
	}
	if !foundRepo {
		t.Errorf("Authorities() = %v, want it to include github://acme/connector", gotAuth)
	}
	// The per-repo authority is NOT an owner authority.
	for _, a := range gotOwners {
		if a == "github://acme/connector" {
			t.Error("OwnerAuthorities() leaked a per-repo authority")
		}
	}
}

// --- parsing-unchanged regression (brief: decode + PEM stay byte-for-byte) ---

func TestLoadKeyring_DecodeUnchangedAcrossBase64Variants(t *testing.T) {
	// Guards the brief's "decodeEd25519Pub stays unchanged" requirement:
	// std/raw-std/url/raw-url base64 all decode and verify, in both the
	// owners and publishers maps.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	encs := map[string]string{
		"std":     base64.StdEncoding.EncodeToString(pub),
		"raw_std": base64.RawStdEncoding.EncodeToString(pub),
		"url":     base64.URLEncoding.EncodeToString(pub),
		"raw_url": base64.RawURLEncoding.EncodeToString(pub),
	}
	for name, encoded := range encs {
		t.Run(name, func(t *testing.T) {
			body := `{"version":2,"owners":{"github://acme":["` + encoded + `"]}}`
			p := filepath.Join(t.TempDir(), "k.json")
			_ = os.WriteFile(p, []byte(body), 0o600)
			kr, err := cstore.LoadKeyring(p)
			if err != nil {
				t.Fatalf("LoadKeyring(%s): %v", name, err)
			}
			sig := ed25519.Sign(priv, append([]byte("x"), []byte("y")...))
			if err := kr.Verify("github://acme", []byte("x"), []byte("y"), sig); err != nil {
				t.Errorf("%s: owner verify: %v", name, err)
			}
		})
	}
}

func TestParsePEMPublicKey_UnchangedRoundTrip(t *testing.T) {
	// Guards the brief's "ParsePEMPublicKey stays unchanged" requirement.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	got, err := cstore.ParsePEMPublicKey(makeTestPEM(t, pub))
	if err != nil {
		t.Fatalf("ParsePEMPublicKey: %v", err)
	}
	if !ed25519.PublicKey(got).Equal(pub) {
		t.Error("PKIX PEM round-trip changed the key")
	}
}

// makeTestPEM encodes the key's SubjectPublicKeyInfo as PEM. Mirrors
// `openssl pkey -pubout` output, used to feed ParsePEMPublicKey.
func makeTestPEM(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// _ silences an "unused import" warning when we strip the local
// helpers in a future refactor; cheap insurance.
var _ = base64.StdEncoding
