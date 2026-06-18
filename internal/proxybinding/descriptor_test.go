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
}
