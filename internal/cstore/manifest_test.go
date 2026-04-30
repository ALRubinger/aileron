package cstore

import (
	"errors"
	"strings"
	"testing"
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
