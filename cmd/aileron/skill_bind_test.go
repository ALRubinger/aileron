package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/credreq"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// fakeBindVault is an in-memory bindVaultClient: it records PutUser calls and
// answers ListUserServices from its own state, so a bind test never touches a
// live daemon or the operator's real vault.
type fakeBindVault struct {
	services map[string][]byte
	puts     int
	listErr  error
	putErr   error
}

func (f *fakeBindVault) ListUserServices() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, 0, len(f.services))
	for k := range f.services {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeBindVault) PutUser(service string, value []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.services == nil {
		f.services = map[string][]byte{}
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	f.services[service] = cp
	f.puts++
	return nil
}

// withBindSeams points the verb at a temp store + temp descriptor file, injects
// the canned requirements, and wires the fake vault. It restores every seam on
// cleanup so a bind test leaves no global state behind.
func withBindSeams(t *testing.T, reqs []credreq.RequiredBinding, vault *fakeBindVault) string {
	t.Helper()
	withTempStore(t)

	descPath := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	origDesc := skillBindDescriptorPath
	skillBindDescriptorPath = descPath
	t.Cleanup(func() { skillBindDescriptorPath = origDesc })

	origDerive := deriveRequirements
	deriveRequirements = func(_ *store.Store, _, _ string) ([]credreq.RequiredBinding, error) { return reqs, nil }
	t.Cleanup(func() { deriveRequirements = origDerive })

	origClient := newBindVaultClient
	newBindVaultClient = func() bindVaultClient { return vault }
	t.Cleanup(func() { newBindVaultClient = origClient })

	return descPath
}

// seedFrozen writes a minimal frozen version into the temp store so
// resolveFrozenVersion resolves a real id. The deriver seam ignores it, but the
// verb still runs the real store resolution the plan reuses.
func seedFrozen(t *testing.T, name, id string) {
	t.Helper()
	s := store.New(skillStoreDir)
	if err := s.WriteFrozen(name, installFrozen(id)); err != nil {
		t.Fatalf("seed frozen: %v", err)
	}
}

// withSecret makes the hidden secret prompt return the given value and returns
// a pointer to the call count.
func withSecret(t *testing.T, secret string) *int {
	t.Helper()
	calls := new(int)
	orig := promptPassphrase
	promptPassphrase = func(_ string, _ io.Writer) (string, error) {
		*calls++
		return secret, nil
	}
	t.Cleanup(func() { promptPassphrase = orig })
	return calls
}

// withNoPrompt fails the test if the hidden secret prompt fires.
func withNoPrompt(t *testing.T) {
	t.Helper()
	orig := promptPassphrase
	promptPassphrase = func(_ string, _ io.Writer) (string, error) {
		t.Fatal("promptPassphrase must not be called when every requirement is satisfied")
		return "", nil
	}
	t.Cleanup(func() { promptPassphrase = orig })
}

// parseDescriptor reads and parses the descriptor file the verb wrote.
func parseDescriptor(t *testing.T, path string) proxybinding.Descriptor {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read descriptor %q: %v", path, err)
	}
	d, err := proxybinding.Parse(data)
	if err != nil {
		t.Fatalf("parse descriptor %q: %v\n%s", path, err, data)
	}
	return d
}

func sigv4Req(label, host string) credreq.RequiredBinding {
	return credreq.RequiredBinding{
		CredentialKind: "aws-sigv4",
		IdentityLabel:  label,
		Scheme:         "sigv4-resign",
		Hosts:          []string{host},
		HostShape:      credreq.HostShapeHostLessIdentity,
		CredentialRef:  "user/" + label,
	}
}

func bearerReq(service string, hosts ...string) credreq.RequiredBinding {
	return credreq.RequiredBinding{
		CredentialKind: "oauth2",
		IdentityLabel:  service,
		Scheme:         "bearer",
		Hosts:          hosts,
		HostShape:      credreq.HostShapeHostKeyed,
		CredentialRef:  "user/" + service,
	}
}

// TestRunSkillBindFullyMissingSigv4 proves a fully-missing sigv4 requirement is
// onboarded: the secret lands in the vault, the descriptor gains a valid
// identity entry with the access key ID and no host, and the summary + restart
// reminder are printed.
func TestRunSkillBindFullyMissingSigv4(t *testing.T) {
	vault := &fakeBindVault{}
	descPath := withBindSeams(t, []credreq.RequiredBinding{sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com")}, vault)
	seedFrozen(t, "rubber-duck", "v1")
	secretCalls := withSecret(t, "supersecret")

	var out, errb bytes.Buffer
	code := runSkillBind([]string{"rubber-duck"}, strings.NewReader("AKIAIOSFODNN7EXAMPLE\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if *secretCalls != 1 {
		t.Errorf("secret prompted %d times, want 1", *secretCalls)
	}
	if vault.puts != 1 || string(vault.services["prod-reader"]) != "supersecret" {
		t.Errorf("vault = %+v, want one put of user/prod-reader=supersecret", vault.services)
	}

	d := parseDescriptor(t, descPath)
	if len(d.Bindings) != 1 {
		t.Fatalf("descriptor has %d bindings, want 1: %+v", len(d.Bindings), d.Bindings)
	}
	e := d.Bindings[0]
	if e.Kind != "aws-sigv4" || e.IdentityLabel != "prod-reader" || e.Scheme != "sigv4-resign" {
		t.Errorf("entry identity = %+v, want aws-sigv4/prod-reader/sigv4-resign", e)
	}
	if e.AccessKeyID != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("access_key_id = %q, want the prompted key", e.AccessKeyID)
	}
	if e.Host != "" {
		t.Errorf("sigv4 entry carries host %q, want none", e.Host)
	}
	if e.CredentialRef != "user/prod-reader" {
		t.Errorf("credential_ref = %q, want user/prod-reader", e.CredentialRef)
	}
	if !strings.Contains(out.String(), "onboarded: user/prod-reader") {
		t.Errorf("stdout = %q, want an onboarded summary line", out.String())
	}
	if !strings.Contains(out.String(), "#1887") || !strings.Contains(out.String(), "restart") {
		t.Errorf("stdout = %q, want a load-once restart reminder", out.String())
	}
}

// TestRunSkillBindPartiallySatisfied proves only the missing requirement is
// filled: a pre-satisfied sigv4 entry is left intact while a missing bearer
// requirement is prompted and written.
func TestRunSkillBindPartiallySatisfied(t *testing.T) {
	vault := &fakeBindVault{services: map[string][]byte{"prod-reader": []byte("existing")}}
	reqs := []credreq.RequiredBinding{
		sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com"),
		bearerReq("workspace-a", "api.example.com"),
	}
	descPath := withBindSeams(t, reqs, vault)
	seedFrozen(t, "rubber-duck", "v1")

	// Pre-seed the descriptor with the satisfied sigv4 entry.
	if err := proxybinding.Upsert(descPath, proxybinding.Entry{
		Kind:          "aws-sigv4",
		IdentityLabel: "prod-reader",
		CredentialRef: "user/prod-reader",
		Scheme:        "sigv4-resign",
		AccessKeyID:   "AKIAIOSFODNN7EXAMPLE",
	}); err != nil {
		t.Fatalf("pre-seed descriptor: %v", err)
	}
	before, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatal(err)
	}

	secretCalls := withSecret(t, "bearer-token")
	var out, errb bytes.Buffer
	// No stdin needed: bearer has no visible prompt.
	code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if *secretCalls != 1 {
		t.Errorf("secret prompted %d times, want 1 (only the missing bearer)", *secretCalls)
	}
	if vault.puts != 1 || string(vault.services["workspace-a"]) != "bearer-token" {
		t.Errorf("vault puts = %d services=%+v, want one put of workspace-a", vault.puts, vault.services)
	}
	// The pre-satisfied prod-reader secret is untouched (never re-prompted).
	if string(vault.services["prod-reader"]) != "existing" {
		t.Errorf("prod-reader secret = %q, want the untouched existing value", vault.services["prod-reader"])
	}

	d := parseDescriptor(t, descPath)
	if len(d.Bindings) != 2 {
		t.Fatalf("descriptor has %d bindings, want 2: %+v", len(d.Bindings), d.Bindings)
	}
	// The satisfied sigv4 entry's content is preserved.
	if !strings.Contains(string(before), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("pre-seed sanity: expected the sigv4 key in the seeded file")
	}
	foundSigv4, foundBearer := false, false
	for _, e := range d.Bindings {
		if e.Kind == "aws-sigv4" && e.AccessKeyID == "AKIAIOSFODNN7EXAMPLE" {
			foundSigv4 = true
		}
		if e.Host == "api.example.com" && e.CredentialRef == "user/workspace-a" && e.Scheme == "bearer" {
			foundBearer = true
		}
	}
	if !foundSigv4 {
		t.Error("satisfied sigv4 entry was rewritten or lost")
	}
	if !foundBearer {
		t.Error("missing bearer entry was not written")
	}
	if !strings.Contains(out.String(), "satisfied: user/prod-reader") {
		t.Errorf("stdout = %q, want prod-reader marked satisfied", out.String())
	}
	if !strings.Contains(out.String(), "onboarded: user/workspace-a") {
		t.Errorf("stdout = %q, want workspace-a marked onboarded", out.String())
	}
}

// TestRunSkillBindSharedCredentialRefPromptsOnce proves two host-keyed
// requirements that share one CredentialRef (same identity, distinct host sets,
// which the deriver keeps as two bindings) prompt for the shared secret once and
// still write both host entries.
func TestRunSkillBindSharedCredentialRefPromptsOnce(t *testing.T) {
	vault := &fakeBindVault{}
	reqs := []credreq.RequiredBinding{
		bearerReq("workspace-a", "api.one.example.com"),
		bearerReq("workspace-a", "api.two.example.com"),
	}
	descPath := withBindSeams(t, reqs, vault)
	seedFrozen(t, "rubber-duck", "v1")
	secretCalls := withSecret(t, "shared-token")

	var out, errb bytes.Buffer
	code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if *secretCalls != 1 {
		t.Errorf("secret prompted %d times, want 1 for the shared ref", *secretCalls)
	}
	if vault.puts != 1 {
		t.Errorf("vault puts = %d, want 1 for the shared ref", vault.puts)
	}
	d := parseDescriptor(t, descPath)
	hosts := map[string]bool{}
	for _, e := range d.Bindings {
		hosts[e.Host] = true
	}
	if !hosts["api.one.example.com"] || !hosts["api.two.example.com"] {
		t.Errorf("descriptor hosts = %v, want both api.one and api.two", hosts)
	}
}

// TestRunSkillBindFullySatisfied proves an already-onboarded plan prompts
// nothing and rewrites no descriptor.
func TestRunSkillBindFullySatisfied(t *testing.T) {
	vault := &fakeBindVault{services: map[string][]byte{"workspace-a": []byte("tok")}}
	descPath := withBindSeams(t, []credreq.RequiredBinding{bearerReq("workspace-a", "api.example.com")}, vault)
	seedFrozen(t, "rubber-duck", "v1")

	if err := proxybinding.Upsert(descPath, proxybinding.Entry{
		Host:          "api.example.com",
		CredentialRef: "user/workspace-a",
		Scheme:        "bearer",
	}); err != nil {
		t.Fatalf("pre-seed descriptor: %v", err)
	}
	before, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatal(err)
	}

	withNoPrompt(t)
	var out, errb bytes.Buffer
	code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if vault.puts != 0 {
		t.Errorf("vault puts = %d, want 0 (nothing missing)", vault.puts)
	}
	after, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("descriptor was rewritten:\nbefore=%s\nafter=%s", before, after)
	}
	if !strings.Contains(out.String(), "satisfied: user/workspace-a") {
		t.Errorf("stdout = %q, want the satisfied summary", out.String())
	}
}

// TestRunSkillBindSigv4ShapeWarning proves a bad-shaped access key ID is warned
// about (warn-only) yet still written, and a well-shaped key produces no
// warning.
func TestRunSkillBindSigv4ShapeWarning(t *testing.T) {
	t.Run("bad shape warns but writes", func(t *testing.T) {
		vault := &fakeBindVault{}
		descPath := withBindSeams(t, []credreq.RequiredBinding{sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com")}, vault)
		seedFrozen(t, "rubber-duck", "v1")
		withSecret(t, "s")
		var out, errb bytes.Buffer
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader("not-a-real-key\n"), &out, &errb)
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, errb.String())
		}
		if !strings.Contains(errb.String(), "access key ID shape") {
			t.Errorf("stderr = %q, want a shape warning", errb.String())
		}
		d := parseDescriptor(t, descPath)
		if len(d.Bindings) != 1 || d.Bindings[0].AccessKeyID != "not-a-real-key" {
			t.Errorf("entry = %+v, want the bad key still written (warn-only)", d.Bindings)
		}
	})

	t.Run("valid shape is clean", func(t *testing.T) {
		vault := &fakeBindVault{}
		withBindSeams(t, []credreq.RequiredBinding{sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com")}, vault)
		seedFrozen(t, "rubber-duck", "v1")
		withSecret(t, "s")
		var out, errb bytes.Buffer
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader("AKIAIOSFODNN7EXAMPLE\n"), &out, &errb)
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, errb.String())
		}
		if strings.Contains(errb.String(), "access key ID shape") {
			t.Errorf("stderr = %q, want no shape warning for a valid key", errb.String())
		}
	})
}

// TestRunSkillBindTemplatedHostAdvisory proves a host-keyed requirement whose
// only host carries an input template is surfaced as an advisory and writes no
// descriptor entry for that host.
func TestRunSkillBindTemplatedHostAdvisory(t *testing.T) {
	vault := &fakeBindVault{}
	req := credreq.RequiredBinding{
		CredentialKind: "oauth2",
		IdentityLabel:  "workspace-a",
		Scheme:         "bearer",
		Hosts:          []string{"athena.{{ inputs.aws_region }}.amazonaws.com"},
		TemplatedHosts: []string{"athena.{{ inputs.aws_region }}.amazonaws.com"},
		HostShape:      credreq.HostShapeHostKeyed,
		CredentialRef:  "user/workspace-a",
	}
	descPath := withBindSeams(t, []credreq.RequiredBinding{req}, vault)
	seedFrozen(t, "rubber-duck", "v1")
	withSecret(t, "tok")

	var out, errb bytes.Buffer
	code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "advisory:") || !strings.Contains(out.String(), "input template") {
		t.Errorf("stdout = %q, want a templated-host advisory", out.String())
	}
	// No descriptor entry was written for the templated host, so the file was
	// never created (nothing to Upsert).
	if _, err := os.Stat(descPath); !os.IsNotExist(err) {
		d := parseDescriptor(t, descPath)
		for _, e := range d.Bindings {
			if strings.Contains(e.Host, "{{") {
				t.Errorf("wrote a descriptor entry for a templated host: %+v", e)
			}
		}
	}
}

// TestRunSkillBindEmptyRequirements proves a plan with no derived requirements
// exits 0 with a "nothing to onboard" message and never prompts.
func TestRunSkillBindEmptyRequirements(t *testing.T) {
	vault := &fakeBindVault{}
	withBindSeams(t, nil, vault)
	seedFrozen(t, "rubber-duck", "v1")
	withNoPrompt(t)
	var out, errb bytes.Buffer
	code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "nothing to onboard") {
		t.Errorf("stdout = %q, want a nothing-to-onboard message", out.String())
	}
	if vault.puts != 0 {
		t.Errorf("vault puts = %d, want 0", vault.puts)
	}
}

// TestRunSkillBindNoFrozenVersion proves the verb surfaces the shared
// resolveFrozenVersion error when the skill has no frozen versions.
func TestRunSkillBindNoFrozenVersion(t *testing.T) {
	withTempStore(t)
	var out, errb bytes.Buffer
	code := runSkillBind([]string{"never-frozen"}, strings.NewReader(""), &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "no frozen versions") {
		t.Errorf("stderr = %q, want the no-frozen-versions error", errb.String())
	}
}

// TestRunSkillBindSigv4MissingIdentityLabel proves a sigv4 requirement with an
// empty identity_label is surfaced as a clear per-requirement error rather than
// writing an invalid descriptor.
func TestRunSkillBindSigv4MissingIdentityLabel(t *testing.T) {
	vault := &fakeBindVault{}
	req := credreq.RequiredBinding{
		CredentialKind: "aws-sigv4",
		IdentityLabel:  "",
		Scheme:         "sigv4-resign",
		Hosts:          []string{"athena.us-east-1.amazonaws.com"},
		HostShape:      credreq.HostShapeHostLessIdentity,
		CredentialRef:  "user/aws-sigv4",
	}
	withBindSeams(t, []credreq.RequiredBinding{req}, vault)
	seedFrozen(t, "rubber-duck", "v1")
	withSecret(t, "s")
	var out, errb bytes.Buffer
	code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1, stderr=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "identity_label") {
		t.Errorf("stderr = %q, want an identity_label error", errb.String())
	}
}

// TestDiffRequirements is a direct, table-driven test of the pure diff over the
// (vault-present x descriptor-present) combinations, including host-less
// identity match and host-keyed multi-host (all-present vs one-missing).
func TestDiffRequirements(t *testing.T) {
	sig := sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com")
	multi := bearerReq("workspace-a", "api.one.example.com", "api.two.example.com")

	sigEntry := proxybinding.Entry{Kind: "aws-sigv4", IdentityLabel: "prod-reader", CredentialRef: "user/prod-reader", Scheme: "sigv4-resign", AccessKeyID: "AKIAIOSFODNN7EXAMPLE"}
	hostEntry := func(h string) proxybinding.Entry {
		return proxybinding.Entry{Host: h, CredentialRef: "user/workspace-a", Scheme: "bearer"}
	}

	cases := []struct {
		name          string
		req           credreq.RequiredBinding
		vault         map[string]bool
		existing      []proxybinding.Entry
		wantVault     bool
		wantDesc      bool
		wantSatisfied bool
	}{
		{"sigv4 both present", sig, map[string]bool{"user/prod-reader": true}, []proxybinding.Entry{sigEntry}, true, true, true},
		{"sigv4 vault only", sig, map[string]bool{"user/prod-reader": true}, nil, true, false, false},
		{"sigv4 descriptor only", sig, nil, []proxybinding.Entry{sigEntry}, false, true, false},
		{"sigv4 neither", sig, nil, nil, false, false, false},
		{"host-keyed all hosts present", multi, map[string]bool{"user/workspace-a": true}, []proxybinding.Entry{hostEntry("api.one.example.com"), hostEntry("api.two.example.com")}, true, true, true},
		{"host-keyed one host missing", multi, map[string]bool{"user/workspace-a": true}, []proxybinding.Entry{hostEntry("api.one.example.com")}, true, false, false},
		{"host-keyed case-insensitive host match", multi, nil, []proxybinding.Entry{hostEntry("API.ONE.example.com"), hostEntry("api.two.example.com")}, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diffRequirements([]credreq.RequiredBinding{tc.req}, tc.vault, tc.existing)
			if len(got) != 1 {
				t.Fatalf("got %d statuses, want 1", len(got))
			}
			st := got[0]
			if st.vaultPresent != tc.wantVault {
				t.Errorf("vaultPresent = %v, want %v", st.vaultPresent, tc.wantVault)
			}
			if st.descriptorPresent != tc.wantDesc {
				t.Errorf("descriptorPresent = %v, want %v", st.descriptorPresent, tc.wantDesc)
			}
			if st.satisfied() != tc.wantSatisfied {
				t.Errorf("satisfied = %v, want %v", st.satisfied(), tc.wantSatisfied)
			}
		})
	}
}

// TestRunSkillBindErrorPaths covers the verb's fail-closed error branches: a
// wrong positional count, a deriver error (an unmapped credential kind), a
// vault list failure, a malformed descriptor file, a vault write failure, and
// the interactive-input rejections (empty secret, empty access key ID).
func TestRunSkillBindErrorPaths(t *testing.T) {
	t.Run("wrong positional count", func(t *testing.T) {
		withTempStore(t)
		var out, errb bytes.Buffer
		if code := runSkillBind(nil, strings.NewReader(""), &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
	})

	t.Run("deriver error fails closed", func(t *testing.T) {
		withTempStore(t)
		origDesc := skillBindDescriptorPath
		skillBindDescriptorPath = filepath.Join(t.TempDir(), "d.yaml")
		t.Cleanup(func() { skillBindDescriptorPath = origDesc })
		origDerive := deriveRequirements
		deriveRequirements = func(_ *store.Store, _, _ string) ([]credreq.RequiredBinding, error) {
			return nil, errTest("unmapped credential kind \"mystery\"")
		}
		t.Cleanup(func() { deriveRequirements = origDerive })
		seedFrozen(t, "rubber-duck", "v1")
		var out, errb bytes.Buffer
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
		if code != 1 || !strings.Contains(errb.String(), "unmapped credential kind") {
			t.Fatalf("exit = %d stderr=%q, want 1 with the deriver error", code, errb.String())
		}
	})

	t.Run("vault list failure", func(t *testing.T) {
		vault := &fakeBindVault{listErr: errTest("daemon unreachable")}
		withBindSeams(t, []credreq.RequiredBinding{bearerReq("workspace-a", "api.example.com")}, vault)
		seedFrozen(t, "rubber-duck", "v1")
		var out, errb bytes.Buffer
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
		if code != 1 || !strings.Contains(errb.String(), "daemon unreachable") {
			t.Fatalf("exit = %d stderr=%q, want 1 with the list error", code, errb.String())
		}
	})

	t.Run("malformed descriptor file", func(t *testing.T) {
		vault := &fakeBindVault{}
		descPath := withBindSeams(t, []credreq.RequiredBinding{bearerReq("workspace-a", "api.example.com")}, vault)
		seedFrozen(t, "rubber-duck", "v1")
		if err := os.WriteFile(descPath, []byte("version: v1\nbindings: [ this is: not valid"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
		if code != 1 {
			t.Fatalf("exit = %d, want 1 on a malformed descriptor", code)
		}
	})

	t.Run("vault write failure", func(t *testing.T) {
		vault := &fakeBindVault{putErr: errTest("vault is locked")}
		withBindSeams(t, []credreq.RequiredBinding{bearerReq("workspace-a", "api.example.com")}, vault)
		seedFrozen(t, "rubber-duck", "v1")
		withSecret(t, "tok")
		var out, errb bytes.Buffer
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
		if code != 1 || !strings.Contains(errb.String(), "vault is locked") {
			t.Fatalf("exit = %d stderr=%q, want 1 with the put error", code, errb.String())
		}
	})

	t.Run("empty secret rejected", func(t *testing.T) {
		vault := &fakeBindVault{}
		withBindSeams(t, []credreq.RequiredBinding{bearerReq("workspace-a", "api.example.com")}, vault)
		seedFrozen(t, "rubber-duck", "v1")
		withSecret(t, "")
		var out, errb bytes.Buffer
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader(""), &out, &errb)
		if code != 1 || !strings.Contains(errb.String(), "secret cannot be empty") {
			t.Fatalf("exit = %d stderr=%q, want 1 with the empty-secret error", code, errb.String())
		}
	})

	t.Run("empty access key id rejected", func(t *testing.T) {
		vault := &fakeBindVault{}
		withBindSeams(t, []credreq.RequiredBinding{sigv4Req("prod-reader", "athena.us-east-1.amazonaws.com")}, vault)
		seedFrozen(t, "rubber-duck", "v1")
		withSecret(t, "s")
		var out, errb bytes.Buffer
		// Blank line for the access key ID prompt.
		code := runSkillBind([]string{"rubber-duck"}, strings.NewReader("\n"), &out, &errb)
		if code != 1 || !strings.Contains(errb.String(), "access key ID cannot be empty") {
			t.Fatalf("exit = %d stderr=%q, want 1 with the empty-key error", code, errb.String())
		}
	})
}

// errTest is a tiny error type for injecting seam failures.
type errTest string

func (e errTest) Error() string { return string(e) }

// TestDaemonBindVaultClientListUserServices proves the daemon-backed client
// reads the user-service list from GET /vault/user and returns the bare service
// segments.
func TestDaemonBindVaultClientListUserServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vault/user" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"services": []map[string]string{{"service": "prod-reader"}, {"service": "workspace-a"}},
		})
	}))
	t.Cleanup(srv.Close)
	setBindingBase(t, srv.URL)

	got, err := daemonBindVaultClient{}.ListUserServices()
	if err != nil {
		t.Fatalf("ListUserServices: %v", err)
	}
	if strings.Join(sortedCopy(got), ",") != "prod-reader,workspace-a" {
		t.Errorf("services = %v, want [prod-reader workspace-a]", got)
	}
}

// TestDaemonBindVaultClientListErrors proves the client maps a 503 and an
// unexpected status to clear errors.
func TestDaemonBindVaultClientListErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"unavailable", http.StatusServiceUnavailable, "not configured with a vault"},
		{"unexpected", http.StatusInternalServerError, "server returned 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			setBindingBase(t, srv.URL)
			_, err := daemonBindVaultClient{}.ListUserServices()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestDaemonBindVaultClientPutUnavailable proves a 503 on PUT maps to a clear
// no-vault error.
func TestDaemonBindVaultClientPutUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	setBindingBase(t, srv.URL)
	err := (daemonBindVaultClient{}).PutUser("prod-reader", []byte("s"))
	if err == nil || !strings.Contains(err.Error(), "not configured with a vault") {
		t.Errorf("err = %v, want a no-vault error", err)
	}
}

// TestDaemonBindVaultClientPutUser proves the client PUTs a base64-encoded
// value to the user credential path and maps 204 to success.
func TestDaemonBindVaultClientPutUser(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/vault/user/prod-reader/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	setBindingBase(t, srv.URL)

	if err := (daemonBindVaultClient{}).PutUser("prod-reader", []byte("supersecret")); err != nil {
		t.Fatalf("PutUser: %v", err)
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode put body: %v", err)
	}
	dec, err := base64.StdEncoding.DecodeString(body.Value)
	if err != nil {
		t.Fatalf("value not base64: %v", err)
	}
	if string(dec) != "supersecret" {
		t.Errorf("decoded value = %q, want supersecret", dec)
	}
}

// TestDaemonBindVaultClientPutUserLocked proves a 423 Locked response maps to a
// clear vault-locked error.
func TestDaemonBindVaultClientPutUserLocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusLocked)
	}))
	t.Cleanup(srv.Close)
	setBindingBase(t, srv.URL)

	err := (daemonBindVaultClient{}).PutUser("prod-reader", []byte("s"))
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("err = %v, want a vault-locked error", err)
	}
}

func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestDescriptorSatisfiesTemplatedHostsOnly proves a host-keyed requirement
// whose only hosts are templated is never considered descriptor-satisfied
// (there is nothing writable to match).
func TestDescriptorSatisfiesTemplatedHostsOnly(t *testing.T) {
	req := credreq.RequiredBinding{
		Scheme:         "bearer",
		Hosts:          []string{"athena.{{ inputs.aws_region }}.amazonaws.com"},
		TemplatedHosts: []string{"athena.{{ inputs.aws_region }}.amazonaws.com"},
		HostShape:      credreq.HostShapeHostKeyed,
		CredentialRef:  "user/workspace-a",
	}
	if descriptorSatisfies(req, nil) {
		t.Error("a templated-only host-keyed requirement must not be satisfied")
	}
}

// bindIntegrationFixtureMD is a schema-valid, environment-pinned SKILL.md with
// two credential-bearing tool steps: one aws-sigv4 (host-less identity) and one
// oauth2 (host-keyed). The real credreq deriver yields one RequiredBinding per
// step, so the verb-level integration test can prove the declared identities
// thread through resolveFrozenVersion -> DeriveFromFrozen to the onboarding
// summary. A `kind: action-call` step (as the worked example uses) would carry
// its trust contract on the action, not the step, and never reach the deriver.
const bindIntegrationFixtureMD = `---
name: bind-integration-fixture
description: bind verb integration fixture.
aileron:
  schemaVersion: aileron.flightplan.v1
  environment:
    tools: [aws-cli@2.x]
  inputs: []
  outputs: []
  steps:
    - id: q1
      kind: tool
      command: [athena-query]
      outputs: [rows]
      trustContract:
        credential: { kind: aws-sigv4, placement: signing, identityLabel: prod-reader }
        hosts: ["athena.us-east-1.amazonaws.com"]
        effect: read
        idempotency: { safeToRetry: true }
        audit: { fields: [result] }
    - id: q2
      kind: tool
      command: [file-issue]
      outputs: [issue]
      trustContract:
        credential: { kind: oauth2, placement: header, identityLabel: digest-bot }
        oauth:
          scopes: [issues:write]
          endpoints:
            authorization: https://auth.example.com/oauth/authorize
            token: https://auth.example.com/oauth/token
          refresh: refresh-token
        hosts: ["tracker.example.com"]
        effect: write
        idempotency: { safeToRetry: false, idempotencyKey: true }
        audit: { fields: [result] }
---
# bind integration fixture
`

// TestRunSkillBind_RealDeriveFromFrozen is the thin verb-level integration test
// that exercises the genuine resolveFrozenVersion -> DeriveFromFrozen wiring on a
// signed frozen fixture. Unlike every other bind test in this file, it does NOT
// stub deriveRequirements: it freezes a two-credential fixture with a real
// ed25519 signing key, then runs bind with the real deriver live and asserts the
// declared identities thread through resolveFrozenVersion ->
// credreq.DeriveFromFrozen -> deriveCredentialRef to their derived vault refs in
// the onboarding summary.
func TestRunSkillBind_RealDeriveFromFrozen(t *testing.T) {
	storeDir := withTempStore(t)
	dir := filepath.Join(storeDir, "bind-integration-fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(bindIntegrationFixtureMD), 0o644); err != nil {
		t.Fatal(err)
	}
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)

	var fout, ferr bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "bind-integration-fixture"}, &fout, &ferr); code != 0 {
		t.Fatalf("freeze exit = %d, stderr=%s", code, ferr.String())
	}

	// Point the descriptor + vault seams at test doubles, but leave
	// deriveRequirements (credreq.DeriveFromFrozen) live so the real deriver runs
	// against the just-frozen, signature-verified plan.
	descPath := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	origDesc := skillBindDescriptorPath
	skillBindDescriptorPath = descPath
	t.Cleanup(func() { skillBindDescriptorPath = origDesc })

	vault := &fakeBindVault{}
	origClient := newBindVaultClient
	newBindVaultClient = func() bindVaultClient { return vault }
	t.Cleanup(func() { newBindVaultClient = origClient })

	secretCalls := withSecret(t, "supersecret")

	// The only stdin consumer is the aws-sigv4 (host-less identity) requirement's
	// AWS access key ID prompt; the oauth2 (host-keyed) requirement reads none.
	var out, errb bytes.Buffer
	code := runSkillBind([]string{"bind-integration-fixture"}, strings.NewReader("AKIAIOSFODNN7EXAMPLE\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("bind exit = %d, stderr=%s", code, errb.String())
	}

	// Both declared identities collapse to their deterministic vault refs and are
	// reported onboarded, proving the id flowed through the real frozen plan.
	got := out.String()
	for _, ref := range []string{
		"onboarded: user/prod-reader",
		"onboarded: user/digest-bot",
	} {
		if !strings.Contains(got, ref) {
			t.Errorf("stdout missing %q; got:\n%s", ref, got)
		}
	}
	if *secretCalls != 2 {
		t.Errorf("secret prompted %d times, want 2 (one per distinct credential ref)", *secretCalls)
	}
	if vault.puts != 2 {
		t.Errorf("vault puts = %d, want 2 (one per distinct credential ref)", vault.puts)
	}
}
