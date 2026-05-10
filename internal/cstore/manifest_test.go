package cstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const validManifestTOML = `[connector]
name = "github://aileron/slack"
version = "1.2.0"
provenance_hash = "sha256:abc123"

[capabilities.network]
hosts = ["slack.com:443", "files.slack.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "chat:write,channels:read"

[capabilities.credential.oauth2]
authorize_url = "https://slack.com/oauth/v2/authorize"
token_url = "https://slack.com/api/oauth.v2.access"
client_id = "1234567890.0987654321"
scopes = ["chat:write", "channels:read"]

[capabilities.runtime]
imports = ["wasi:http/outgoing-handler"]

[provides]
intents = ["post_message"]
`

func TestParseManifest_AcceptsCanonicalForm(t *testing.T) {
	m, err := ParseManifest("manifest.toml", []byte(validManifestTOML))
	if err != nil {
		t.Fatalf("ParseManifest err = %v", err)
	}
	if m.Connector.Name != "github://aileron/slack" {
		t.Errorf("Connector.Name = %q", m.Connector.Name)
	}
	if m.Connector.Version != "1.2.0" {
		t.Errorf("Connector.Version = %q", m.Connector.Version)
	}
	if m.Connector.ProvenanceHash != "sha256:abc123" {
		t.Errorf("ProvenanceHash = %q", m.Connector.ProvenanceHash)
	}
	if m.Capabilities.Network == nil || len(m.Capabilities.Network.Hosts) != 2 {
		t.Errorf("Capabilities.Network.Hosts = %v", m.Capabilities.Network)
	}
	if m.Capabilities.Credential == nil || m.Capabilities.Credential.Kind != "oauth2" {
		t.Errorf("Capabilities.Credential = %v", m.Capabilities.Credential)
	}
	if len(m.Provides.Intents) != 1 || m.Provides.Intents[0] != "post_message" {
		t.Errorf("Provides.Intents = %v", m.Provides.Intents)
	}
}

func TestParseManifest_RejectsUnknownKey(t *testing.T) {
	bad := validManifestTOML + "\n[unrecognized]\nfoo = 1\n"
	_, err := ParseManifest("m.toml", []byte(bad))
	if err == nil {
		t.Fatal("ParseManifest accepted unknown key; want error")
	}
	if !strings.Contains(err.Error(), "unrecognized") {
		t.Errorf("error %q does not name the unknown key", err.Error())
	}
}

func TestParseManifest_TOMLSyntaxErrorReportsLine(t *testing.T) {
	bad := `[connector]
name = "x"
version = =
`
	_, err := ParseManifest("bad.toml", []byte(bad))
	if err == nil {
		t.Fatal("ParseManifest accepted bad TOML; want error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassParseError {
		t.Fatalf("expected *Error of ClassParseError, got %v", err)
	}
}

func TestValidateManifest_HappyPath(t *testing.T) {
	m, err := ParseManifest("m.toml", []byte(validManifestTOML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateManifest(m, "m.toml"); err != nil {
		t.Errorf("ValidateManifest err = %v", err)
	}
}

func TestValidateManifest_RejectsBadFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"empty name", func(m *Manifest) { m.Connector.Name = "" }, "name is required"},
		{"bad scheme", func(m *Manifest) { m.Connector.Name = "ftp://owner/repo" }, "scheme"},
		{"empty version", func(m *Manifest) { m.Connector.Version = "" }, "version is required"},
		{"non-semver", func(m *Manifest) { m.Connector.Version = "1.x" }, "strict SemVer"},
		{"bad hash prefix", func(m *Manifest) { m.Connector.ProvenanceHash = "md5:abc" }, "sha256:"},
		{"bad host", func(m *Manifest) { m.Capabilities.Network = &ManifestNetwork{Hosts: []string{"slack.com"}} }, "host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseManifest("m.toml", []byte(validManifestTOML))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tc.mutate(m)
			err = ValidateManifest(m, "m.toml")
			if err == nil {
				t.Fatalf("ValidateManifest accepted; want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.want)
			}
			var aerr *Error
			if !errors.As(err, &aerr) || aerr.Class != ClassValidationError {
				t.Errorf("expected ClassValidationError, got %v", err)
			}
		})
	}
}

func TestValidateManifest_NilManifest(t *testing.T) {
	if err := ValidateManifest(nil, ""); err == nil {
		t.Fatal("ValidateManifest(nil) succeeded; want error")
	}
}

// Idempotency declaration (ADR-0010).

func TestIsIdempotent_DefaultsToTrue(t *testing.T) {
	m, err := ParseManifest("m.toml", []byte(validManifestTOML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !m.IsIdempotent() {
		t.Error("default (no idempotency block) should be idempotent")
	}
}

func TestIsIdempotent_RespectsExplicitIdempotent(t *testing.T) {
	m := &Manifest{Connector: ManifestConnector{
		Idempotency: &ManifestIdempotency{Default: IdempotencyIdempotent},
	}}
	if !m.IsIdempotent() {
		t.Error("explicit idempotent value should be idempotent")
	}
}

func TestIsIdempotent_NotIdempotentOptOut(t *testing.T) {
	m := &Manifest{Connector: ManifestConnector{
		Idempotency: &ManifestIdempotency{Default: IdempotencyNotIdempotent},
	}}
	if m.IsIdempotent() {
		t.Error("not_idempotent declaration should opt out of retry")
	}
}

func TestIsIdempotent_NilManifestSafe(t *testing.T) {
	var m *Manifest
	if !m.IsIdempotent() {
		t.Error("nil manifest should default to idempotent (safe)")
	}
}

func TestValidateManifest_RejectsBadIdempotencyValues(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want string
	}{
		{"empty", "", "is required when the table is present"},
		{"unknown", "maybe", "must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := ParseManifest("m.toml", []byte(validManifestTOML))
			m.Connector.Idempotency = &ManifestIdempotency{Default: tc.val}
			err := ValidateManifest(m, "m.toml")
			if err == nil {
				t.Fatalf("accepted bad value %q", tc.val)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateManifest_AcceptsValidIdempotencyValues(t *testing.T) {
	for _, v := range []string{IdempotencyIdempotent, IdempotencyNotIdempotent} {
		t.Run(v, func(t *testing.T) {
			m, _ := ParseManifest("m.toml", []byte(validManifestTOML))
			m.Connector.Idempotency = &ManifestIdempotency{Default: v}
			if err := ValidateManifest(m, "m.toml"); err != nil {
				t.Errorf("ValidateManifest(%q) err = %v", v, err)
			}
		})
	}
}

func TestParseManifest_AcceptsIdempotencyTOML(t *testing.T) {
	body := validManifestTOML + "\n[connector.idempotency]\ndefault = \"not_idempotent\"\n"
	m, err := ParseManifest("m.toml", []byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Connector.Idempotency == nil {
		t.Fatal("Idempotency block not parsed")
	}
	if m.Connector.Idempotency.Default != IdempotencyNotIdempotent {
		t.Errorf("Default = %q", m.Connector.Idempotency.Default)
	}
}

// --- credential validation tests (#388) ---

func TestValidateManifest_AcceptsAPIKeyKind(t *testing.T) {
	m := canonicalManifestForTest()
	m.Capabilities.Credential = &ManifestCredential{
		Kind:  "api_key",
		Scope: "Read your account",
	}
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateManifest_RejectsAPIKeyWithOAuth2Block(t *testing.T) {
	m := canonicalManifestForTest()
	m.Capabilities.Credential = &ManifestCredential{
		Kind:   "api_key",
		OAuth2: &ManifestOAuth2{},
	}
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected error when api_key kind has [capabilities.credential.oauth2]")
	}
	if !strings.Contains(err.Error(), "must be absent") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateManifest_AcceptsOAuth2WithFullConfig(t *testing.T) {
	m := canonicalManifestForTest()
	m.Capabilities.Credential = &ManifestCredential{
		Kind:  "oauth2",
		Scope: "Read email and send messages",
		OAuth2: &ManifestOAuth2{
			AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL:     "https://oauth2.googleapis.com/token",
			ClientID:     "1234.apps.googleusercontent.com",
			Scopes:       []string{"https://www.googleapis.com/auth/gmail.send"},
		},
	}
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateManifest_RejectsOAuth2WithoutBlock(t *testing.T) {
	m := canonicalManifestForTest()
	m.Capabilities.Credential = &ManifestCredential{Kind: "oauth2"}
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected error when oauth2 kind has no [capabilities.credential.oauth2]")
	}
	if !strings.Contains(err.Error(), "is required") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateManifest_RejectsBadOAuth2Fields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ManifestOAuth2)
		want   string
	}{
		{"missing authorize_url", func(o *ManifestOAuth2) { o.AuthorizeURL = "" },
			"authorize_url is required"},
		{"non-https authorize_url", func(o *ManifestOAuth2) { o.AuthorizeURL = "http://example.com" },
			"must be https://"},
		{"missing token_url", func(o *ManifestOAuth2) { o.TokenURL = "" },
			"token_url is required"},
		{"non-https token_url", func(o *ManifestOAuth2) { o.TokenURL = "http://example.com" },
			"must be https://"},
		{"missing client_id", func(o *ManifestOAuth2) { o.ClientID = "" },
			"client_id is required"},
		{"empty scopes", func(o *ManifestOAuth2) { o.Scopes = nil },
			"scopes is required"},
		{"blank scope entry", func(o *ManifestOAuth2) { o.Scopes = []string{"   "} },
			"is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := canonicalManifestForTest()
			oauth := &ManifestOAuth2{
				AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
				TokenURL:     "https://oauth2.googleapis.com/token",
				ClientID:     "1234",
				Scopes:       []string{"openid"},
			}
			tc.mutate(oauth)
			m.Capabilities.Credential = &ManifestCredential{Kind: "oauth2", OAuth2: oauth}
			err := ValidateManifest(m, "x.toml")
			if err == nil {
				t.Fatalf("Validate accepted; want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateManifest_RejectsUnknownKind(t *testing.T) {
	m := canonicalManifestForTest()
	m.Capabilities.Credential = &ManifestCredential{Kind: "basic"}
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
	if !strings.Contains(err.Error(), "v1 closed set") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateManifest_RejectsEmptyKindWhenCredentialBlockPresent(t *testing.T) {
	m := canonicalManifestForTest()
	m.Capabilities.Credential = &ManifestCredential{Scope: "x"}
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected error when kind is empty")
	}
	if !strings.Contains(err.Error(), "kind is required") {
		t.Errorf("err = %v", err)
	}
}

func TestParseManifest_AcceptsOAuth2TOML(t *testing.T) {
	tomlBody := []byte(`[connector]
name = "github://acme/foo"
version = "1.0.0"
publisher = "acme"

[capabilities.credential]
kind = "oauth2"
scope = "Read your email"

[capabilities.credential.oauth2]
authorize_url = "https://accounts.google.com/o/oauth2/v2/auth"
token_url = "https://oauth2.googleapis.com/token"
client_id = "1234.apps.googleusercontent.com"
scopes = ["https://www.googleapis.com/auth/gmail.send"]
`)
	m, err := ParseManifest("x.toml", tomlBody)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	cred := m.Capabilities.Credential
	if cred == nil || cred.OAuth2 == nil {
		t.Fatal("OAuth2 block not parsed")
	}
	if cred.OAuth2.ClientID != "1234.apps.googleusercontent.com" {
		t.Errorf("ClientID = %q", cred.OAuth2.ClientID)
	}
	if len(cred.OAuth2.Scopes) != 1 {
		t.Errorf("scopes = %v", cred.OAuth2.Scopes)
	}
	if err := ValidateManifest(m, "x.toml"); err != nil {
		t.Errorf("ValidateManifest after Parse: %v", err)
	}
}

// canonicalManifestForTest returns a minimum-valid Manifest the OAuth
// tests mutate. Mirrors goodManifest() in the validate_test.go pattern.
func canonicalManifestForTest() *Manifest {
	return &Manifest{
		Connector: ManifestConnector{
			Name:    "github://acme/foo",
			Version: "1.0.0",
		},
	}
}

func TestValidateManifest_AllowsLoopbackHTTPForLocalDev(t *testing.T) {
	// RFC 8252 §7.3 explicitly allows http for loopback addresses.
	// The validator carves out localhost / 127.0.0.1 so test
	// harnesses (httptest.NewServer) work without TLS gymnastics.
	for _, host := range []string{"http://localhost:1234/auth", "http://127.0.0.1/auth"} {
		t.Run(host, func(t *testing.T) {
			m := canonicalManifestForTest()
			m.Capabilities.Credential = &ManifestCredential{
				Kind: "oauth2",
				OAuth2: &ManifestOAuth2{
					AuthorizeURL: host,
					TokenURL:     "https://provider.test/token",
					ClientID:     "cid",
					Scopes:       []string{"x"},
				},
			}
			if err := ValidateManifest(m, "x.toml"); err != nil {
				t.Errorf("loopback http should be allowed: %v", err)
			}
		})
	}
}

// --- spawn capability validation (#509) ---

func goodSpawn() *ManifestSpawn {
	return &ManifestSpawn{
		Programs:       []ManifestSpawnProgram{{Path: "/usr/bin/git"}},
		ArgvPatterns:   []string{"git log --since={since} --author={author}"},
		EnvPassthrough: []string{"GIT_AUTHOR_NAME"},
		FSRead:         []string{"~/code/"},
		FSWrite:        []string{"~/.cache/aileron/gitcrawl/"},
	}
}

func TestValidateManifest_AcceptsFullSpawnBlock(t *testing.T) {
	m := canonicalManifestForTest()
	m.Capabilities.Spawn = goodSpawn()
	m.Capabilities.Spawn.Cwd = "~/code/"
	m.Capabilities.Spawn.Programs[0].Hash = "sha256:abc123"
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateManifest_AcceptsMinimalSpawnBlock(t *testing.T) {
	// FSRead, FSWrite, EnvPassthrough are optional. The minimum is
	// one program and one argv pattern.
	m := canonicalManifestForTest()
	m.Capabilities.Spawn = &ManifestSpawn{
		Programs:     []ManifestSpawnProgram{{Path: "/usr/bin/git"}},
		ArgvPatterns: []string{"git status"},
	}
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateManifest_AcceptsAbsentSpawnBlock(t *testing.T) {
	// The whole [capabilities.spawn] block is optional — connectors
	// that do not spawn anything simply omit it.
	m := canonicalManifestForTest()
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateManifest_RejectsBadSpawnFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ManifestSpawn)
		want   string
	}{
		{"empty programs", func(s *ManifestSpawn) {
			s.Programs = nil
		}, "programs is required"},
		{"empty program path", func(s *ManifestSpawn) {
			s.Programs = []ManifestSpawnProgram{{Path: ""}}
		}, "path is required"},
		{"whitespace program path", func(s *ManifestSpawn) {
			s.Programs = []ManifestSpawnProgram{{Path: "   "}}
		}, "path is required"},
		{"relative program path", func(s *ManifestSpawn) {
			s.Programs = []ManifestSpawnProgram{{Path: "git"}}
		}, "absolute"},
		{"bad program hash prefix", func(s *ManifestSpawn) {
			s.Programs = []ManifestSpawnProgram{{Path: "/usr/bin/git", Hash: "md5:abc"}}
		}, "sha256:"},
		{"hash prefix only", func(s *ManifestSpawn) {
			s.Programs = []ManifestSpawnProgram{{Path: "/usr/bin/git", Hash: "sha256:"}}
		}, "sha256:"},
		{"empty argv_patterns", func(s *ManifestSpawn) {
			s.ArgvPatterns = nil
		}, "argv_patterns is required"},
		{"blank argv pattern", func(s *ManifestSpawn) {
			s.ArgvPatterns = []string{"   "}
		}, "is empty"},
		{"env key leading digit", func(s *ManifestSpawn) {
			s.EnvPassthrough = []string{"1VAR"}
		}, "valid environment variable"},
		{"env key with dash", func(s *ManifestSpawn) {
			s.EnvPassthrough = []string{"MY-VAR"}
		}, "valid environment variable"},
		{"env key empty", func(s *ManifestSpawn) {
			s.EnvPassthrough = []string{""}
		}, "valid environment variable"},
		{"relative fs_read", func(s *ManifestSpawn) {
			s.FSRead = []string{"code/"}
		}, "absolute"},
		{"relative fs_write", func(s *ManifestSpawn) {
			s.FSWrite = []string{"cache/"}
		}, "absolute"},
		{"relative cwd", func(s *ManifestSpawn) {
			s.Cwd = "code/"
		}, "absolute"},
		{"windows-style cwd", func(s *ManifestSpawn) {
			s.Cwd = `C:\Users\me`
		}, "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := canonicalManifestForTest()
			sp := goodSpawn()
			tc.mutate(sp)
			m.Capabilities.Spawn = sp
			err := ValidateManifest(m, "x.toml")
			if err == nil {
				t.Fatalf("Validate accepted; want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.want)
			}
			var aerr *Error
			if !errors.As(err, &aerr) || aerr.Class != ClassValidationError {
				t.Errorf("expected ClassValidationError, got %v", err)
			}
		})
	}
}

func TestValidateManifest_AcceptsTildeOnlyPath(t *testing.T) {
	// `~` (no trailing slash) is the user's home directory; the
	// validator accepts it as an anchored path.
	m := canonicalManifestForTest()
	sp := goodSpawn()
	sp.FSRead = []string{"~"}
	sp.Cwd = "~"
	m.Capabilities.Spawn = sp
	if err := ValidateManifest(m, "x.toml"); err != nil {
		t.Errorf("`~` should be accepted: %v", err)
	}
}

func TestValidateManifest_AcceptsAbsoluteUnixPaths(t *testing.T) {
	m := canonicalManifestForTest()
	sp := goodSpawn()
	sp.FSRead = []string{"/var/spool/", "/etc/aileron/"}
	sp.FSWrite = []string{"/tmp/aileron/"}
	sp.Cwd = "/tmp/aileron/"
	m.Capabilities.Spawn = sp
	if err := ValidateManifest(m, "x.toml"); err != nil {
		t.Errorf("absolute paths should be accepted: %v", err)
	}
}

func TestParseManifest_AcceptsSpawnTOML(t *testing.T) {
	body := []byte(`[connector]
name = "github://acme/gitcrawl"
version = "1.0.0"

[capabilities.spawn]
argv_patterns = ["git log --since={since}"]
env_passthrough = ["GIT_AUTHOR_NAME"]
fs_read = ["~/code/"]
fs_write = ["~/.cache/aileron/gitcrawl/"]
cwd = "~/code/"

[[capabilities.spawn.programs]]
path = "/usr/bin/git"
hash = "sha256:abc123"
`)
	m, err := ParseManifest("x.toml", body)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	sp := m.Capabilities.Spawn
	if sp == nil {
		t.Fatal("Spawn block not parsed")
	}
	if len(sp.Programs) != 1 || sp.Programs[0].Path != "/usr/bin/git" {
		t.Errorf("Programs = %v", sp.Programs)
	}
	if sp.Programs[0].Hash != "sha256:abc123" {
		t.Errorf("Programs[0].Hash = %q", sp.Programs[0].Hash)
	}
	if len(sp.ArgvPatterns) != 1 || sp.ArgvPatterns[0] != "git log --since={since}" {
		t.Errorf("ArgvPatterns = %v", sp.ArgvPatterns)
	}
	if len(sp.EnvPassthrough) != 1 || sp.EnvPassthrough[0] != "GIT_AUTHOR_NAME" {
		t.Errorf("EnvPassthrough = %v", sp.EnvPassthrough)
	}
	if sp.Cwd != "~/code/" {
		t.Errorf("Cwd = %q", sp.Cwd)
	}
	if err := ValidateManifest(m, "x.toml"); err != nil {
		t.Errorf("ValidateManifest after Parse: %v", err)
	}
}

func TestParseManifest_SpawnRoundTrip(t *testing.T) {
	// Round-trip property: a manifest with [capabilities.spawn] parses,
	// validates, then re-encodes to TOML that parses back identically.
	// Mirrors the parse/validate property expected of every capability
	// block per issue #509's acceptance criterion.
	body := []byte(`[connector]
name = "github://acme/gitcrawl"
version = "1.0.0"

[capabilities.spawn]
argv_patterns = ["git status", "git log --since={since}"]
env_passthrough = ["GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"]
fs_read = ["~/code/", "~/.gitconfig"]
fs_write = ["~/.cache/aileron/gitcrawl/"]

[[capabilities.spawn.programs]]
path = "/usr/bin/git"
hash = "sha256:bd6e"
`)
	m1, err := ParseManifest("a.toml", body)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if err := ValidateManifest(m1, "a.toml"); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	var buf strings.Builder
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(m1); err != nil {
		t.Fatalf("encode: %v", err)
	}
	m2, err := ParseManifest("b.toml", []byte(buf.String()))
	if err != nil {
		t.Fatalf("second parse: %v\nencoded:\n%s", err, buf.String())
	}
	if err := ValidateManifest(m2, "b.toml"); err != nil {
		t.Fatalf("second validate: %v", err)
	}
	if !spawnEqual(m1.Capabilities.Spawn, m2.Capabilities.Spawn) {
		t.Errorf("round-trip diverged:\n  before: %+v\n  after:  %+v",
			m1.Capabilities.Spawn, m2.Capabilities.Spawn)
	}
}

func spawnEqual(a, b *ManifestSpawn) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Programs) != len(b.Programs) {
		return false
	}
	for i := range a.Programs {
		if a.Programs[i] != b.Programs[i] {
			return false
		}
	}
	return stringSliceEqual(a.ArgvPatterns, b.ArgvPatterns) &&
		stringSliceEqual(a.EnvPassthrough, b.EnvPassthrough) &&
		stringSliceEqual(a.FSRead, b.FSRead) &&
		stringSliceEqual(a.FSWrite, b.FSWrite) &&
		a.Cwd == b.Cwd
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIsAbsoluteOrTildePath(t *testing.T) {
	cases := map[string]bool{
		"/usr/bin/git":         true,
		"/":                    true,
		"~":                    true,
		"~/code":               true,
		"~/code/":              true,
		"":                     false,
		"git":                  false,
		"./bin/git":            false,
		"../bin/git":           false,
		`C:\Program Files\git`: false,
		"~user/code":           false,
	}
	for p, want := range cases {
		if got := isAbsoluteOrTildePath(p); got != want {
			t.Errorf("isAbsoluteOrTildePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestIsAllowedOAuthURL(t *testing.T) {
	cases := map[string]bool{
		"https://provider.test/auth":      true,
		"https://accounts.google.com/x":   true,
		"http://localhost/auth":           true,
		"http://localhost:8080/auth":      true,
		"http://127.0.0.1/auth":           true,
		"http://127.0.0.1:1234/auth":      true,
		"http://example.com/auth":         false,
		"ftp://provider.test/x":           false,
		"":                                false,
		"not a url":                       false,
		"http://localhost.attacker.com/x": false, // host-prefix attack
	}
	for url, want := range cases {
		if got := isAllowedOAuthURL(url); got != want {
			t.Errorf("isAllowedOAuthURL(%q) = %v, want %v", url, got, want)
		}
	}
}
