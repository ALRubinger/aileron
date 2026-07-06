package credreq

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/credential/inject"
)

// TestSchemeFor_MappedKinds proves every mapped credential kind returns the
// documented scheme and host-shape from the closed table. These are the
// load-bearing rows: aws-sigv4 is the sigv4-resign / host-less case the brief
// names, oauth2 and api-key are the host-keyed bearer cases.
func TestSchemeFor_MappedKinds(t *testing.T) {
	cases := []struct {
		kind       string
		wantScheme string
		wantShape  HostShape
	}{
		{"aws-sigv4", string(inject.SchemeSigV4Resign), HostShapeHostLessIdentity},
		{"oauth2", string(inject.SchemeBearer), HostShapeHostKeyed},
		{"api-key", string(inject.SchemeBearer), HostShapeHostKeyed},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			scheme, shape, skip, err := schemeFor(tc.kind)
			if err != nil {
				t.Fatalf("schemeFor(%q) errored: %v", tc.kind, err)
			}
			if skip {
				t.Fatalf("schemeFor(%q) reported skip; a mapped credential kind must not skip", tc.kind)
			}
			if scheme != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tc.wantScheme)
			}
			if shape != tc.wantShape {
				t.Errorf("shape = %q, want %q", shape, tc.wantShape)
			}
		})
	}
}

// TestSchemeFor_MappedSchemesAreClosedSet proves every scheme the table emits
// is a member of the injector's closed scheme set, so the derived scheme can
// never drift outside inject.AllSchemes().
func TestSchemeFor_MappedSchemesAreClosedSet(t *testing.T) {
	valid := map[string]bool{}
	for _, s := range inject.AllSchemes() {
		valid[string(s)] = true
	}
	for kind, m := range kindSchemeTable {
		if m.skip {
			continue
		}
		if !valid[m.scheme] {
			t.Errorf("kind %q maps to scheme %q, which is not a member of inject.AllSchemes()", kind, m.scheme)
		}
	}
}

// TestSchemeFor_NoneSkips proves the unauthenticated `none` kind reports
// skip=true: it onboards no credential and yields no requirement.
func TestSchemeFor_NoneSkips(t *testing.T) {
	_, _, skip, err := schemeFor("none")
	if err != nil {
		t.Fatalf("schemeFor(none) errored: %v", err)
	}
	if !skip {
		t.Fatal("schemeFor(none) must report skip=true")
	}
}

// TestSchemeFor_UnmappedErrors proves a kind outside the closed table fails
// closed: an error naming the offending kind and the mapped set, never a
// guessed scheme.
func TestSchemeFor_UnmappedErrors(t *testing.T) {
	scheme, _, skip, err := schemeFor("exotic-kind")
	if err == nil {
		t.Fatal("an unmapped credential kind must return an error")
	}
	if scheme != "" || skip {
		t.Errorf("unmapped kind returned scheme=%q skip=%v, want empty/false", scheme, skip)
	}
	if !strings.Contains(err.Error(), "exotic-kind") {
		t.Errorf("error %q should name the offending kind", err)
	}
	// The error names the closed mapped set so an author can correct the kind.
	for _, k := range []string{"aws-sigv4", "oauth2", "api-key", "none"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q should list the mapped kind %q", err, k)
		}
	}
}
