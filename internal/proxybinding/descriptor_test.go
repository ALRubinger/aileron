package proxybinding

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

// linearDescriptorYAML is the verbatim-Authorization Linear case: a
// header-template scheme whose template is the bare "{token}", producing
// "Authorization: <key>" with no Bearer prefix.
const linearDescriptorYAML = `
version: v1
bindings:
  - host: api.linear.app
    credential_ref: user/linear
    scheme: header-template
    emit_mechanism: A
    header: Authorization
    template: "{token}"
`

func TestParse_LinearHeaderTemplateRoundTrips(t *testing.T) {
	d, err := Parse([]byte(linearDescriptorYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Version != SchemaVersion {
		t.Errorf("version = %q, want %q", d.Version, SchemaVersion)
	}
	if len(d.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(d.Bindings))
	}
	e := d.Bindings[0]
	if e.Host != "api.linear.app" {
		t.Errorf("host = %q, want api.linear.app", e.Host)
	}
	if e.Scheme != binding.SchemeHeaderTemplate {
		t.Errorf("scheme = %q, want header-template", e.Scheme)
	}
	if e.Header != "Authorization" {
		t.Errorf("header = %q, want Authorization", e.Header)
	}
	// The verbatim shape: template is the bare token slot, no "Bearer ".
	if e.Template != "{token}" {
		t.Errorf("template = %q, want %q (no Bearer prefix)", e.Template, "{token}")
	}
	if strings.Contains(strings.ToLower(e.Template), "bearer") {
		t.Errorf("template = %q must not carry a Bearer prefix", e.Template)
	}
}

func TestParse_EachSchemeValidates(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "bearer",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n",
			want: binding.SchemeBearer,
		},
		{
			name: "basic",
			yaml: "version: v1\nbindings:\n  - host: git.example.com\n    credential_ref: user/example\n    scheme: basic\n    username: x-access-token\n",
			want: binding.SchemeBasic,
		},
		{
			name: "header-template",
			yaml: "version: v1\nbindings:\n  - host: api.linear.app\n    credential_ref: user/linear\n    scheme: header-template\n    header: Authorization\n    template: \"{token}\"\n",
			want: binding.SchemeHeaderTemplate,
		},
		{
			name: "query-param",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: query-param\n    query_param: api_key\n",
			want: binding.SchemeQueryParam,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := d.Bindings[0].Scheme; got != tc.want {
				t.Errorf("scheme = %q, want %q", got, tc.want)
			}
			if _, err := d.Bindings[0].ToHostBinding(); err != nil {
				t.Errorf("ToHostBinding: %v", err)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "missing host",
			yaml: "version: v1\nbindings:\n  - credential_ref: user/example\n    scheme: bearer\n",
		},
		{
			name: "missing credential_ref",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    scheme: bearer\n",
		},
		{
			name: "missing scheme",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n",
		},
		{
			name: "unknown scheme",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: oauth-magic\n",
		},
		{
			name: "unknown emit_mechanism",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    emit_mechanism: C\n",
		},
		{
			// Mechanism B requires a sentinel block; a bare "B" with no
			// sentinel is a load-time error (fail-closed).
			name: "emit_mechanism B without sentinel block",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    emit_mechanism: B\n",
		},
		{
			// Mechanism B with a sentinel block missing its value.
			name: "emit_mechanism B with empty sentinel.value",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    emit_mechanism: B\n    sentinel:\n      env: GH_TOKEN\n",
		},
		{
			// Mechanism B with a sentinel block missing its env.
			name: "emit_mechanism B with empty sentinel.env",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    emit_mechanism: B\n    sentinel:\n      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA\n",
		},
		{
			// A stray sentinel on an explicit mechanism-A binding is an error.
			name: "emit_mechanism A with a stray sentinel block",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    emit_mechanism: A\n    sentinel:\n      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA\n      env: GH_TOKEN\n",
		},
		{
			// A stray sentinel on a defaulted (omitted) mechanism-A binding
			// is an error.
			name: "defaulted mechanism A with a stray sentinel block",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    sentinel:\n      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA\n      env: GH_TOKEN\n",
		},
		{
			// Strict decode recurses into the nested sentinel mapping: an
			// unknown key inside the block is a load-time error.
			name: "unknown key nested under sentinel",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    emit_mechanism: B\n    sentinel:\n      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA\n      env: GH_TOKEN\n      bogus: nope\n",
		},
		{
			name: "header-template missing header",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: header-template\n    template: \"{token}\"\n",
		},
		{
			name: "header-template missing template",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: header-template\n    header: Authorization\n",
		},
		{
			name: "basic missing username",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: basic\n",
		},
		{
			name: "query-param missing param",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: query-param\n",
		},
		{
			name: "invalid credential_ref",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: not a ref\n    scheme: bearer\n",
		},
		{
			name: "invalid host pattern",
			yaml: "version: v1\nbindings:\n  - host: \"*.com\"\n    credential_ref: user/example\n    scheme: bearer\n",
		},
		{
			name: "unknown yaml key",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n    bogus_field: nope\n",
		},
		{
			name: "wrong version",
			yaml: "version: v2\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n",
		},
		{
			name: "missing version",
			yaml: "bindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n",
		},
		{
			name: "malformed yaml",
			yaml: "version: v1\nbindings: [this is: not valid",
		},
		{
			name: "duplicate host in one descriptor",
			yaml: "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n  - host: api.example.com\n    credential_ref: user/other\n    scheme: bearer\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml)); err == nil {
				t.Fatalf("Parse(%s) = nil error, want error", tc.name)
			}
		})
	}
}

// TestParse_EmitMechanismBSentinelSchema pins the mechanism-B schema this
// issue freezes (#1246). A well-formed mechanism-B descriptor declares a
// sentinel block with both value and env, and those fields round-trip onto
// the parsed Entry. A mechanism-B descriptor missing the block or either
// field is rejected, and a mechanism-A descriptor that carries a stray
// sentinel is rejected. Mechanism "A" and the empty default still parse.
// This is the fail-closed contract; it is intentionally decoupled from the
// table-driven cases so the policy is explicit and survives refactors.
func TestParse_EmitMechanismBSentinelSchema(t *testing.T) {
	const base = "version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: bearer\n"

	// "A" and the empty default are accepted with no sentinel.
	if _, err := Parse([]byte(base + "    emit_mechanism: A\n")); err != nil {
		t.Errorf("emit_mechanism A: Parse = %v, want nil", err)
	}
	if _, err := Parse([]byte(base)); err != nil {
		t.Errorf("emit_mechanism omitted (defaults to A): Parse = %v, want nil", err)
	}

	// A well-formed mechanism-B descriptor parses and the sentinel fields
	// round-trip onto the Entry.
	const wantValue = "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"
	const wantEnv = "GH_TOKEN"
	bYAML := base + "    emit_mechanism: " + string(binding.EmitMechanismB) + "\n" +
		"    sentinel:\n      value: " + wantValue + "\n      env: " + wantEnv + "\n"
	d, err := Parse([]byte(bYAML))
	if err != nil {
		t.Fatalf("well-formed mechanism B: Parse = %v, want nil", err)
	}
	e := d.Bindings[0]
	if e.EmitMechanism != string(binding.EmitMechanismB) {
		t.Errorf("emit_mechanism = %q, want %q", e.EmitMechanism, string(binding.EmitMechanismB))
	}
	if e.Sentinel == nil {
		t.Fatalf("sentinel block = nil, want populated")
	}
	if e.Sentinel.Value != wantValue {
		t.Errorf("sentinel.value = %q, want %q", e.Sentinel.Value, wantValue)
	}
	if e.Sentinel.Env != wantEnv {
		t.Errorf("sentinel.env = %q, want %q", e.Sentinel.Env, wantEnv)
	}

	// The canonical constant is the value mechanism-B descriptors declare.
	if string(binding.EmitMechanismB) != "B" {
		t.Errorf("binding.EmitMechanismB = %q, want %q", string(binding.EmitMechanismB), "B")
	}
}

// A descriptor carries only a credential reference; no descriptor field
// resembles a secret. This guards the fail-closed property that the loader
// never reads secret bytes.
func TestParse_NoSecretBytesInDescriptor(t *testing.T) {
	d, err := Parse([]byte(linearDescriptorYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := d.Bindings[0]
	// The credential-ref is a vault path, never a token.
	if !strings.HasPrefix(e.CredentialRef, "user/") {
		t.Errorf("credential_ref = %q, expected a vault reference, not a secret", e.CredentialRef)
	}

	// A mechanism-B sentinel value is a recognizable placeholder, not a
	// secret: it embeds the reserved "AILERONSENTINEL" marker so a reader
	// (or a log) can tell at a glance it carries no authority.
	const bYAML = "version: v1\nbindings:\n  - host: api.github.com\n    credential_ref: user/github\n    scheme: bearer\n    emit_mechanism: B\n    sentinel:\n      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA\n      env: GH_TOKEN\n"
	db, err := Parse([]byte(bYAML))
	if err != nil {
		t.Fatalf("Parse mechanism B: %v", err)
	}
	sentinel := db.Bindings[0].Sentinel
	if sentinel == nil {
		t.Fatalf("sentinel = nil, want populated")
	}
	if !strings.Contains(sentinel.Value, "AILERONSENTINEL") {
		t.Errorf("sentinel.value = %q, expected a recognizable placeholder, not a secret", sentinel.Value)
	}
}
