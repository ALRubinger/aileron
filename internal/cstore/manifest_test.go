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
		Programs: []ManifestSpawnProgram{{Path: "/usr/bin/git"}},
		Operations: map[string]ManifestSpawnOperation{
			"log": {Argv: "git log --since={since} --author={author}"},
		},
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
	// one program and one operation.
	m := canonicalManifestForTest()
	m.Capabilities.Spawn = &ManifestSpawn{
		Programs: []ManifestSpawnProgram{{Path: "/usr/bin/git"}},
		Operations: map[string]ManifestSpawnOperation{
			"status": {Argv: "git status"},
		},
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
		{"empty operations", func(s *ManifestSpawn) {
			s.Operations = nil
		}, "operations] is required"},
		{"blank operation argv", func(s *ManifestSpawn) {
			s.Operations = map[string]ManifestSpawnOperation{"x": {Argv: "   "}}
		}, "argv is required"},
		{"invalid op name", func(s *ManifestSpawn) {
			s.Operations = map[string]ManifestSpawnOperation{"BadName": {Argv: "git status"}}
		}, "name must match"},
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

func TestValidateManifest_AcceptsSpawnLimits(t *testing.T) {
	m := canonicalManifestForTest()
	sp := goodSpawn()
	stdoutCap := int64(2 << 20) // 2 MiB
	stderrCap := int64(64 << 10)
	sp.Limits = &ManifestSpawnLimits{
		MaxStdoutBytes: &stdoutCap,
		MaxStderrBytes: &stderrCap,
	}
	m.Capabilities.Spawn = sp
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	if got := sp.StdoutCap(); got != stdoutCap {
		t.Errorf("StdoutCap() = %d, want %d", got, stdoutCap)
	}
	if got := sp.StderrCap(); got != stderrCap {
		t.Errorf("StderrCap() = %d, want %d", got, stderrCap)
	}
}

func TestSpawnCapsFallBackToDefaultsWhenLimitsAbsent(t *testing.T) {
	// Contract: when no limits block is declared, the runtime applies
	// the default caps (per ADR-0014).
	sp := goodSpawn()
	if got := sp.StdoutCap(); got != DefaultMaxStdoutBytes {
		t.Errorf("StdoutCap() = %d, want default %d", got, DefaultMaxStdoutBytes)
	}
	if got := sp.StderrCap(); got != DefaultMaxStderrBytes {
		t.Errorf("StderrCap() = %d, want default %d", got, DefaultMaxStderrBytes)
	}
}

func TestSpawnCapsFallBackPerStreamWhenOneOmitted(t *testing.T) {
	// Contract: declaring one stream's cap does not affect the other,
	// which falls back to its default.
	sp := goodSpawn()
	stdoutCap := int64(4 << 20)
	sp.Limits = &ManifestSpawnLimits{MaxStdoutBytes: &stdoutCap}
	if got := sp.StdoutCap(); got != stdoutCap {
		t.Errorf("StdoutCap() = %d, want %d", got, stdoutCap)
	}
	if got := sp.StderrCap(); got != DefaultMaxStderrBytes {
		t.Errorf("StderrCap() = %d, want default %d", got, DefaultMaxStderrBytes)
	}
}

func TestValidateManifest_RejectsBadSpawnLimits(t *testing.T) {
	zero := int64(0)
	negative := int64(-1)
	overCap := MaxSpawnOutputCap + 1
	cases := []struct {
		name   string
		limits ManifestSpawnLimits
		want   string
	}{
		{"stdout zero", ManifestSpawnLimits{MaxStdoutBytes: &zero}, "max_stdout_bytes"},
		{"stdout negative", ManifestSpawnLimits{MaxStdoutBytes: &negative}, "max_stdout_bytes"},
		{"stdout over cap", ManifestSpawnLimits{MaxStdoutBytes: &overCap}, "exceeds the maximum"},
		{"stderr zero", ManifestSpawnLimits{MaxStderrBytes: &zero}, "max_stderr_bytes"},
		{"stderr negative", ManifestSpawnLimits{MaxStderrBytes: &negative}, "max_stderr_bytes"},
		{"stderr over cap", ManifestSpawnLimits{MaxStderrBytes: &overCap}, "exceeds the maximum"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := canonicalManifestForTest()
			sp := goodSpawn()
			lim := tc.limits
			sp.Limits = &lim
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

func TestParseManifest_AcceptsSpawnLimitsTOML(t *testing.T) {
	body := []byte(`[connector]
name = "github://acme/gitcrawl"
version = "1.0.0"

[capabilities.spawn]

[[capabilities.spawn.programs]]
path = "/usr/bin/git"

[capabilities.spawn.operations.log]
argv = "git log"

[capabilities.spawn.limits]
max_stdout_bytes = 2097152
max_stderr_bytes = 65536
`)
	m, err := ParseManifest("x.toml", body)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := ValidateManifest(m, "x.toml"); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
	sp := m.Capabilities.Spawn
	if sp.Limits == nil {
		t.Fatal("Limits block not parsed")
	}
	if sp.StdoutCap() != 2<<20 {
		t.Errorf("StdoutCap() = %d, want %d", sp.StdoutCap(), 2<<20)
	}
	if sp.StderrCap() != 64<<10 {
		t.Errorf("StderrCap() = %d, want %d", sp.StderrCap(), 64<<10)
	}
}

func TestValidateManifest_AcceptsCredentialEnvKeys(t *testing.T) {
	m := canonicalManifestForTest()
	sp := goodSpawn()
	sp.EnvPassthrough = []string{"GIT_AUTHOR_NAME", "GH_TOKEN"}
	sp.CredentialEnvKeys = []string{"GH_TOKEN"}
	m.Capabilities.Spawn = sp
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateManifest_RejectsCredentialKeyNotInPassthrough(t *testing.T) {
	m := canonicalManifestForTest()
	sp := goodSpawn()
	sp.EnvPassthrough = []string{"GIT_AUTHOR_NAME"}
	sp.CredentialEnvKeys = []string{"GH_TOKEN"} // not in env_passthrough
	m.Capabilities.Spawn = sp
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected denial when credential key is not in env_passthrough")
	}
	if !strings.Contains(err.Error(), "env_passthrough") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateManifest_RejectsInvalidCredentialKeyName(t *testing.T) {
	m := canonicalManifestForTest()
	sp := goodSpawn()
	sp.EnvPassthrough = []string{"1BAD"}
	sp.CredentialEnvKeys = []string{"1BAD"}
	m.Capabilities.Spawn = sp
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected denial for invalid env name")
	}
	// First error surfaces from env_passthrough validation.
	if !strings.Contains(err.Error(), "valid environment variable") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateManifest_AcceptsForwarderConnector(t *testing.T) {
	// A connector that opts into the daemon-embedded forwarder declares
	// connector.forwarder plus a normal spawn block. Validation accepts.
	m := canonicalManifestForTest()
	m.Connector.Forwarder = BuiltinForwarderSpawn
	m.Capabilities.Spawn = goodSpawn()
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateManifest_RejectsUnknownForwarder(t *testing.T) {
	m := canonicalManifestForTest()
	m.Connector.Forwarder = "builtin://something-else"
	m.Capabilities.Spawn = goodSpawn()
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected error for unknown forwarder")
	}
	if !strings.Contains(err.Error(), "not recognized") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateManifest_RejectsForwarderWithoutSpawn(t *testing.T) {
	// The forwarder needs a spawn capability to drive; setting it
	// without [capabilities.spawn] is meaningless.
	m := canonicalManifestForTest()
	m.Connector.Forwarder = BuiltinForwarderSpawn
	err := ValidateManifest(m, "ok.toml")
	if err == nil {
		t.Fatal("expected error when forwarder is set but no spawn block")
	}
	if !strings.Contains(err.Error(), "[capabilities.spawn]") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateManifest_AcceptsForwarderFreeSpawnConnector(t *testing.T) {
	// A connector can use spawn without opting into the forwarder
	// (e.g. a custom WASM body that calls aileron_host.spawn directly).
	m := canonicalManifestForTest()
	m.Capabilities.Spawn = goodSpawn()
	// Forwarder field intentionally absent.
	if err := ValidateManifest(m, "ok.toml"); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestManifestSpawn_ArgvPatternsDerivedFromOperations(t *testing.T) {
	s := &ManifestSpawn{
		Operations: map[string]ManifestSpawnOperation{
			"log":    {Argv: "git log --since={since}"},
			"status": {Argv: "git status"},
		},
	}
	got := s.ArgvPatterns()
	if len(got) != 2 {
		t.Fatalf("ArgvPatterns() len = %d, want 2", len(got))
	}
	// Map iteration order is unstable; assert both entries appear.
	seen := map[string]bool{}
	for _, p := range got {
		seen[p] = true
	}
	if !seen["git log --since={since}"] || !seen["git status"] {
		t.Errorf("ArgvPatterns() = %v; want both entries", got)
	}
}

func TestManifestSpawn_ArgvPatternsHandlesNil(t *testing.T) {
	var s *ManifestSpawn
	if got := s.ArgvPatterns(); got != nil {
		t.Errorf("nil receiver should return nil; got %v", got)
	}
	empty := &ManifestSpawn{}
	if got := empty.ArgvPatterns(); got != nil {
		t.Errorf("empty Operations should return nil; got %v", got)
	}
}

func TestOpNameRegex_AcceptsValidNames(t *testing.T) {
	good := []string{"log", "list", "list_recent", "send-msg", "x1"}
	for _, n := range good {
		if !opNameRe.MatchString(n) {
			t.Errorf("opNameRe rejected valid name %q", n)
		}
	}
	bad := []string{"", "Log", "1log", "log!", "_internal", "Send-Msg"}
	for _, n := range bad {
		if opNameRe.MatchString(n) {
			t.Errorf("opNameRe accepted invalid name %q", n)
		}
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
env_passthrough = ["GIT_AUTHOR_NAME"]
fs_read = ["~/code/"]
fs_write = ["~/.cache/aileron/gitcrawl/"]
cwd = "~/code/"

[[capabilities.spawn.programs]]
path = "/usr/bin/git"
hash = "sha256:abc123"

[capabilities.spawn.operations.log]
argv = "git log --since={since}"
description = "List commits since a date"
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
	logOp, ok := sp.Operations["log"]
	if !ok {
		t.Fatalf("Operations missing 'log': %v", sp.Operations)
	}
	if logOp.Argv != "git log --since={since}" {
		t.Errorf("Operations.log.argv = %q", logOp.Argv)
	}
	if logOp.Description != "List commits since a date" {
		t.Errorf("Operations.log.description = %q", logOp.Description)
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
env_passthrough = ["GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"]
fs_read = ["~/code/", "~/.gitconfig"]
fs_write = ["~/.cache/aileron/gitcrawl/"]

[[capabilities.spawn.programs]]
path = "/usr/bin/git"
hash = "sha256:bd6e"

[capabilities.spawn.operations.log]
argv = "git log --since={since}"

[capabilities.spawn.operations.status]
argv = "git status"
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
	if len(a.Operations) != len(b.Operations) {
		return false
	}
	for name, opA := range a.Operations {
		opB, ok := b.Operations[name]
		if !ok || opA != opB {
			return false
		}
	}
	return stringSliceEqual(a.EnvPassthrough, b.EnvPassthrough) &&
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
