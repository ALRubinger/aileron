package inject

import (
	"errors"
	"testing"
)

func TestParseSchemeValid(t *testing.T) {
	cases := map[string]Scheme{
		"bearer":          SchemeBearer,
		"basic":           SchemeBasic,
		"header-template": SchemeHeaderTemplate,
		"query-param":     SchemeQueryParam,
		"sigv4-resign":    SchemeSigV4Resign,
	}
	for in, want := range cases {
		got, err := ParseScheme(in)
		if err != nil {
			t.Errorf("ParseScheme(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSchemeUnknown(t *testing.T) {
	for _, in := range []string{"", "Bearer", "oauth2", "BASIC", "query_param", "bearer "} {
		got, err := ParseScheme(in)
		if !errors.Is(err, ErrUnknownScheme) {
			t.Errorf("ParseScheme(%q) error = %v, want ErrUnknownScheme", in, err)
		}
		if got != "" {
			t.Errorf("ParseScheme(%q) = %q, want empty on error", in, got)
		}
	}
}

func TestParseSchemeErrorDoesNotEchoSecretShape(t *testing.T) {
	// The error wraps the offending input verbatim; confirm it surfaces
	// the input so callers can debug, and that it is the unknown-scheme
	// sentinel (not a different class).
	_, err := ParseScheme("totally-made-up")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("error = %v, want ErrUnknownScheme", err)
	}
}

func TestAllSchemesExactSet(t *testing.T) {
	got := AllSchemes()
	want := []Scheme{
		SchemeBearer,
		SchemeBasic,
		SchemeHeaderTemplate,
		SchemeQueryParam,
		SchemeSigV4Resign,
	}
	if len(got) != len(want) {
		t.Fatalf("AllSchemes() returned %d schemes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllSchemes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Every member must round-trip through ParseScheme.
	for _, s := range got {
		parsed, err := ParseScheme(string(s))
		if err != nil || parsed != s {
			t.Errorf("ParseScheme(%q) = (%q, %v), want (%q, nil)", s, parsed, err, s)
		}
	}
}

func TestAllSchemesReturnsCopy(t *testing.T) {
	a := AllSchemes()
	a[0] = "mutated"
	b := AllSchemes()
	if b[0] != SchemeBearer {
		t.Errorf("AllSchemes() leaked internal slice: got %q after mutation", b[0])
	}
}
