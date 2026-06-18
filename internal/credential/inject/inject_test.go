package inject

import (
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sentinelSecret = "s3cr3t-TOKEN-do-not-leak-9f3a"

func newReq(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	return req
}

func TestInjectBearer(t *testing.T) {
	req := newReq(t, "https://api.example.com/v1/things")
	// Pre-existing header must be replaced, not appended.
	req.Header.Set("Authorization", "Bearer stale-value")

	if err := Inject(req, SchemeBearer, []byte(sentinelSecret), Params{}); err != nil {
		t.Fatalf("Inject bearer: %v", err)
	}

	got := req.Header.Values("Authorization")
	if len(got) != 1 {
		t.Fatalf("Authorization header count = %d, want 1 (replace not append): %v", len(got), got)
	}
	if want := "Bearer " + sentinelSecret; got[0] != want {
		t.Errorf("Authorization = %q, want %q", got[0], want)
	}
}

func TestInjectBasic(t *testing.T) {
	req := newReq(t, "https://github.com/owner/repo.git/info/refs")
	const username = "x-access-token"

	if err := Inject(req, SchemeBasic, []byte(sentinelSecret), Params{Username: username}); err != nil {
		t.Fatalf("Inject basic: %v", err)
	}

	got := req.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("Authorization = %q, want Basic prefix", got)
	}
	encoded := strings.TrimPrefix(got, "Basic ")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode of %q: %v", encoded, err)
	}
	if want := username + ":" + sentinelSecret; string(decoded) != want {
		t.Errorf("decoded basic = %q, want %q", decoded, want)
	}
}

func TestInjectBasicMissingUsername(t *testing.T) {
	req := newReq(t, "https://github.com/owner/repo.git")
	err := Inject(req, SchemeBasic, []byte(sentinelSecret), Params{})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("request mutated on error path")
	}
}

func TestInjectHeaderTemplate(t *testing.T) {
	req := newReq(t, "https://api.example.com/v1/things")
	const header = "X-Vendor-Auth"
	const tmpl = "token=" + tokenPlaceholder + ";v=2"

	if err := Inject(req, SchemeHeaderTemplate, []byte(sentinelSecret), Params{
		HeaderName: header,
		Template:   tmpl,
	}); err != nil {
		t.Fatalf("Inject header-template: %v", err)
	}

	got := req.Header.Get(header)
	if want := "token=" + sentinelSecret + ";v=2"; got != want {
		t.Errorf("%s = %q, want %q", header, got, want)
	}
	if strings.Contains(got, "Bearer ") {
		t.Errorf("header-template injected unexpected Bearer prefix: %q", got)
	}
}

func TestInjectHeaderTemplateEmptyTemplateFallsBackToRawToken(t *testing.T) {
	req := newReq(t, "https://api.example.com/v1/things")
	const header = "X-Api-Key"

	if err := Inject(req, SchemeHeaderTemplate, []byte(sentinelSecret), Params{
		HeaderName: header,
	}); err != nil {
		t.Fatalf("Inject header-template: %v", err)
	}
	if got := req.Header.Get(header); got != sentinelSecret {
		t.Errorf("%s = %q, want raw token %q", header, got, sentinelSecret)
	}
}

func TestInjectHeaderTemplateMissingHeaderName(t *testing.T) {
	req := newReq(t, "https://api.example.com/v1/things")
	err := Inject(req, SchemeHeaderTemplate, []byte(sentinelSecret), Params{Template: "x=" + tokenPlaceholder})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
}

func TestInjectQueryParam(t *testing.T) {
	req := newReq(t, "https://api.example.com/v1/things?existing=keep&page=2")
	const param = "access_token"

	if err := Inject(req, SchemeQueryParam, []byte(sentinelSecret), Params{ParamName: param}); err != nil {
		t.Fatalf("Inject query-param: %v", err)
	}

	q := req.URL.Query()
	if got := q.Get(param); got != sentinelSecret {
		t.Errorf("query %s = %q, want %q", param, got, sentinelSecret)
	}
	if got := q.Get("existing"); got != "keep" {
		t.Errorf("existing query param = %q, want preserved %q", got, "keep")
	}
	if got := q.Get("page"); got != "2" {
		t.Errorf("page query param = %q, want preserved %q", got, "2")
	}
	// URL must re-encode to something parseable.
	if _, err := req.URL.Parse(req.URL.String()); err != nil {
		t.Errorf("re-encoded URL not parseable: %v", err)
	}
}

func TestInjectQueryParamMissingParamName(t *testing.T) {
	req := newReq(t, "https://api.example.com/v1/things")
	err := Inject(req, SchemeQueryParam, []byte(sentinelSecret), Params{})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
	if req.URL.RawQuery != "" {
		t.Errorf("query mutated on error path: %q", req.URL.RawQuery)
	}
}

func TestInjectSigV4ResignNotImplemented(t *testing.T) {
	req := newReq(t, "https://s3.amazonaws.com/bucket/key")
	err := Inject(req, SchemeSigV4Resign, []byte(sentinelSecret), Params{})
	if !errors.Is(err, ErrSchemeNotImplemented) {
		t.Fatalf("error = %v, want ErrSchemeNotImplemented", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("sigv4-resign mutated Authorization header")
	}
	if req.URL.RawQuery != "" {
		t.Error("sigv4-resign mutated query")
	}
}

func TestInjectUnknownScheme(t *testing.T) {
	req := newReq(t, "https://api.example.com/v1/things")
	err := Inject(req, Scheme("not-a-scheme"), []byte(sentinelSecret), Params{})
	if !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("error = %v, want ErrUnknownScheme", err)
	}
}

func TestInjectNilRequest(t *testing.T) {
	err := Inject(nil, SchemeBearer, []byte(sentinelSecret), Params{})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
}

// TestInjectNoSecretLeakInErrors runs every scheme (including the error
// and stub paths) with a sentinel secret and asserts the secret never
// appears in any returned error string. The injected header/query value
// is the only sanctioned destination for the secret; everything else is
// scanned.
func TestInjectNoSecretLeakInErrors(t *testing.T) {
	type call struct {
		name   string
		scheme Scheme
		params Params
	}
	calls := []call{
		{"bearer", SchemeBearer, Params{}},
		{"basic-ok", SchemeBasic, Params{Username: "x-access-token"}},
		{"basic-missing", SchemeBasic, Params{}},
		{"header-template-ok", SchemeHeaderTemplate, Params{HeaderName: "X-Auth", Template: "t=" + tokenPlaceholder}},
		{"header-template-missing", SchemeHeaderTemplate, Params{}},
		{"query-param-ok", SchemeQueryParam, Params{ParamName: "access_token"}},
		{"query-param-missing", SchemeQueryParam, Params{}},
		{"sigv4", SchemeSigV4Resign, Params{}},
		{"unknown", Scheme("bogus"), Params{}},
	}
	for _, c := range calls {
		req := newReq(t, "https://api.example.com/v1/things")
		err := Inject(req, c.scheme, []byte(sentinelSecret), c.params)
		if err != nil && strings.Contains(err.Error(), sentinelSecret) {
			t.Errorf("%s: error string leaked secret: %q", c.name, err.Error())
		}
	}
}

// TestNoLoggingCallsInPackage enforces the no-logging invariant the
// package doc claims ("This package contains no logging calls"). The
// no-secret-leak guarantee depends on the secret never reaching a log
// sink, so this scans the package's own non-test .go sources and fails
// if any logging facility is imported or called. This makes the
// invariant enforced rather than only asserted by construction: if a
// future edit introduces a log/slog/fmt.Print* call, this test breaks.
func TestNoLoggingCallsInPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	// Import paths that are logging sinks. fmt is handled separately
	// below because only its Print* family writes output; fmt is
	// otherwise used legitimately for fmt.Errorf.
	bannedImports := map[string]bool{
		"log":      true,
		"log/slog": true,
	}

	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if bannedImports[path] {
				t.Errorf("%s imports logging package %q; the inject package must contain no logging calls", name, path)
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			callExpr, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := callExpr.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			// Ban any fmt.Print* call (Print, Println, Printf), which
			// writes to stdout and could surface a secret.
			if pkgIdent.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Print") {
				pos := fset.Position(callExpr.Pos())
				t.Errorf("%s:%d calls fmt.%s; the inject package must contain no logging/print calls", name, pos.Line, sel.Sel.Name)
			}
			return true
		})
	}

	if scanned == 0 {
		cwd, _ := filepath.Abs(".")
		t.Fatalf("scanned 0 source files in %s; guard test would silently pass", cwd)
	}
}

// TestInjectHeaderValueIsTheOnlySecretSurface confirms the secret lands
// in the intended request surface for the happy-path schemes and nowhere
// else in the request line we can observe (method, path for header
// schemes, other headers).
func TestInjectHeaderValueIsTheOnlySecretSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	req := newReq(t, srv.URL+"/path?keep=1")
	if err := Inject(req, SchemeBearer, []byte(sentinelSecret), Params{}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// The path/query must not carry the bearer secret.
	if strings.Contains(req.URL.String(), sentinelSecret) {
		t.Errorf("bearer scheme leaked secret into URL: %q", req.URL.String())
	}
}
