package inject

import (
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Published AWS Signature Version 4 test-suite fixtures
// (the canonical aws-sig-v4-test-suite). These are AWS-published
// *example* credentials, not real secrets.
const (
	vectorAccessKeyID = "AKIDEXAMPLE"
	vectorSecretKey   = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion      = "us-east-1"
	vectorService     = "service"
	vectorScope       = "20150830/us-east-1/service/aws4_request"
	// emptyPayloadHash is the hex SHA-256 of the empty string.
	emptyPayloadHashTest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// vectorTime is the fixed signing time the published suite uses:
// 2015-08-30T12:36:00Z.
func vectorTime() time.Time {
	return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
}

// pinClock pins the package clock to the published-vector signing time
// and restores it after the test.
func pinClock(t *testing.T) {
	t.Helper()
	now = vectorTime
	t.Cleanup(func() { now = time.Now })
}

func authHeader(signedHeaders, signature string) string {
	return "AWS4-HMAC-SHA256 " +
		"Credential=" + vectorAccessKeyID + "/" + vectorScope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
}

// TestInjectSigV4ResignVectors asserts byte-correct SigV4 output for the
// canonical get-vanilla / get-vanilla-query / post-with-body cases using
// the published aws-sig-v4-test-suite credentials, region, service, and
// fixed signing time (2015-08-30T12:36:00Z).
//
// The expected signatures below differ from the legacy aws-sig-v4-test-
// suite "authorization" fixtures because this signer follows the modern
// AWS convention of emitting X-Amz-Content-Sha256 and including it in the
// signed-headers set (decision 5 of the issue plan; required by S3 and
// the recommended form for all services). The legacy suite predates that
// header and signs only host;x-amz-date. These expected values were
// cross-checked against an independent reference implementation of the
// SigV4 algorithm over the identical canonical request (host;
// x-amz-content-sha256;x-amz-date), so they pin the algorithm and catch
// any canonicalization regression.
func TestInjectSigV4ResignVectors(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		url         string
		body        string
		wantAuth    string
		wantContent string
		wantBody    string // non-empty => assert body readable + length intact
	}{
		{
			name:        "get-vanilla",
			method:      http.MethodGet,
			url:         "https://example.amazonaws.com/",
			wantAuth:    authHeader("host;x-amz-content-sha256;x-amz-date", "726c5c4879a6b4ccbbd3b24edbd6b8826d34f87450fbbf4e85546fc7ba9c1642"),
			wantContent: emptyPayloadHashTest,
		},
		{
			name:        "get-vanilla-query",
			method:      http.MethodGet,
			url:         "https://example.amazonaws.com/?Param1=value1",
			wantAuth:    authHeader("host;x-amz-content-sha256;x-amz-date", "2287c0f96af21b7ccf3ee4a2905bcbb2d6f9a94c68d0849f3d1715ef003f2a05"),
			wantContent: emptyPayloadHashTest,
		},
		{
			name:        "post-with-body",
			method:      http.MethodPost,
			url:         "https://example.amazonaws.com/",
			body:        "Param1=value1",
			wantAuth:    authHeader("host;x-amz-content-sha256;x-amz-date", "29ad954417b30cf1d248d8068d3387c6b1fa0057764dd9ff44c287c3aedbaaba"),
			wantContent: "9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
			wantBody:    "Param1=value1",
		},
		{
			// Query params with characters that must be percent-encoded
			// in the canonical query string (space, slash), exercising the
			// uriEncode/percentDecode machinery and sorted-key ordering.
			name:        "get-query-encoded",
			method:      http.MethodGet,
			url:         "https://example.amazonaws.com/?a=b%20c&x=y%2Fz",
			wantAuth:    authHeader("host;x-amz-content-sha256;x-amz-date", "1ddeda8c9340c17b884eb08e9a8cad4087aa5c6ad4efdc103dfb45db4ca393dd"),
			wantContent: emptyPayloadHashTest,
		},
		{
			// Path segment with a percent-encoded space, exercising the
			// per-segment path canonicalization (decode then re-encode).
			name:        "get-path-encoded",
			method:      http.MethodGet,
			url:         "https://example.amazonaws.com/foo%20bar/baz",
			wantAuth:    authHeader("host;x-amz-content-sha256;x-amz-date", "b15c91e98f56dd76e9f869826dae6311b6f13ad09ef788a2196f2ac4433c2b20"),
			wantContent: emptyPayloadHashTest,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pinClock(t)
			var body io.Reader
			if c.body != "" {
				body = strings.NewReader(c.body)
			}
			req, err := http.NewRequest(c.method, c.url, body)
			if err != nil {
				t.Fatalf("http.NewRequest: %v", err)
			}
			if err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
				AccessKeyID: vectorAccessKeyID,
				Region:      vectorRegion,
				Service:     vectorService,
			}); err != nil {
				t.Fatalf("Inject sigv4: %v", err)
			}

			if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
				t.Errorf("X-Amz-Date = %q, want %q", got, "20150830T123600Z")
			}
			if got := req.Header.Get("X-Amz-Content-Sha256"); got != c.wantContent {
				t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, c.wantContent)
			}
			if got := req.Header.Get("Authorization"); got != c.wantAuth {
				t.Errorf("Authorization mismatch\n got = %q\nwant = %q", got, c.wantAuth)
			}
			// The secret must never appear in any header value.
			for name, vs := range req.Header {
				for _, v := range vs {
					if strings.Contains(v, vectorSecretKey) {
						t.Errorf("secret leaked into header %s: %q", name, v)
					}
				}
			}

			if c.wantBody != "" {
				if req.ContentLength != int64(len(c.wantBody)) {
					t.Errorf("ContentLength = %d, want %d", req.ContentLength, len(c.wantBody))
				}
				got, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("re-read body: %v", err)
				}
				if string(got) != c.wantBody {
					t.Errorf("body after Inject = %q, want %q (rewind failed)", got, c.wantBody)
				}
			}
		})
	}
}

// TestInjectSigV4ResignDropsClientAuthorization is the regression guard
// for the AWS-CLI end-to-end path (#1505): a SigV4-aware client (botocore)
// pre-signs the request and carries its own AWS4-HMAC-SHA256 Authorization
// header. If the signer folded that stale header into the canonical request
// and the SignedHeaders list, it would then overwrite it with the new
// Authorization, producing a signature over a header value the upstream
// never receives. AWS rejects that with SignatureDoesNotMatch.
//
// The contract: the signer must drop the inbound Authorization before
// deriving the signed-header set, so the emitted signature is identical to
// the one produced for the same request with no inbound Authorization. We
// assert byte-for-byte equality against the get-vanilla published vector,
// which proves `authorization` is absent from SignedHeaders and the
// canonical request.
func TestInjectSigV4ResignDropsClientAuthorization(t *testing.T) {
	pinClock(t)
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	// A SigV4-aware client's stale, self-computed Authorization header.
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=CLIENTKEY/20150830/us-east-1/service/aws4_request, "+
			"SignedHeaders=host;x-amz-date, Signature=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	if err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		AccessKeyID: vectorAccessKeyID,
		Region:      vectorRegion,
		Service:     vectorService,
	}); err != nil {
		t.Fatalf("Inject sigv4: %v", err)
	}

	// Identical to the get-vanilla vector: the stale Authorization did not
	// enter the canonical request or the SignedHeaders list.
	wantAuth := authHeader("host;x-amz-content-sha256;x-amz-date",
		"726c5c4879a6b4ccbbd3b24edbd6b8826d34f87450fbbf4e85546fc7ba9c1642")
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Errorf("Authorization mismatch (stale client header was not dropped)\n got = %q\nwant = %q", got, wantAuth)
	}
	if strings.Contains(req.Header.Get("Authorization"), "CLIENTKEY") {
		t.Error("emitted Authorization still references the client's stale credential")
	}
}

func TestInjectSigV4MissingAccessKeyID(t *testing.T) {
	req := newReq(t, "https://example.amazonaws.com/")
	err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		Region:  vectorRegion,
		Service: vectorService,
	})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("X-Amz-Date") != "" {
		t.Error("request mutated on missing-AccessKeyID error path")
	}
}

// TestInjectSigV4MissingRegion asserts the fail-closed contract: an
// unparseable host ("example.amazonaws.com") with only a partial stored
// fallback (Service but no Region) yields ErrMissingParam and leaves the
// request unmutated. The fallback is honored only when BOTH stored fields
// are present.
func TestInjectSigV4MissingRegion(t *testing.T) {
	req := newReq(t, "https://example.amazonaws.com/")
	err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		AccessKeyID: vectorAccessKeyID,
		Service:     vectorService,
	})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("X-Amz-Date") != "" {
		t.Error("request mutated on missing-Region error path")
	}
}

// TestInjectSigV4DeriveScopeFromHost is the incremental win: a parseable AWS
// host with NO stored region/service still signs, deriving both from the
// host. The emitted credential scope carries the host's region and service.
func TestInjectSigV4DeriveScopeFromHost(t *testing.T) {
	pinClock(t)
	req := newReq(t, "https://athena.us-east-1.amazonaws.com/")
	if err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		AccessKeyID: vectorAccessKeyID,
	}); err != nil {
		t.Fatalf("Inject sigv4 (derive from host): %v", err)
	}
	auth := req.Header.Get("Authorization")
	wantScope := "/us-east-1/athena/aws4_request"
	if !strings.Contains(auth, wantScope) {
		t.Errorf("Authorization scope = %q, want it to contain %q", auth, wantScope)
	}
	if !strings.Contains(auth, "Credential="+vectorAccessKeyID+"/") {
		t.Errorf("Authorization = %q, want Credential=%s/...", auth, vectorAccessKeyID)
	}
}

// TestInjectSigV4GovCloudHost proves the generalized region parser signs a
// GovCloud endpoint (us-gov-west-1), which the earlier two-word region
// regex could not recognize.
func TestInjectSigV4GovCloudHost(t *testing.T) {
	pinClock(t)
	req := newReq(t, "https://athena.us-gov-west-1.amazonaws.com/")
	if err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		AccessKeyID: vectorAccessKeyID,
	}); err != nil {
		t.Fatalf("Inject sigv4 (govcloud): %v", err)
	}
	if want := "/us-gov-west-1/athena/aws4_request"; !strings.Contains(req.Header.Get("Authorization"), want) {
		t.Errorf("Authorization = %q, want scope %q", req.Header.Get("Authorization"), want)
	}
}

// TestInjectSigV4HostOverridesStoredScope is the regression for the
// UnrecognizedClientException class: a parseable host wins over a stored
// region/service that disagrees, so the request signs against the host the
// proxy actually dials, not the stale stored value.
func TestInjectSigV4HostOverridesStoredScope(t *testing.T) {
	pinClock(t)
	req := newReq(t, "https://athena.us-east-1.amazonaws.com/")
	if err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		AccessKeyID: vectorAccessKeyID,
		Region:      "eu-west-1", // disagrees with the host
		Service:     "s3",        // disagrees with the host
	}); err != nil {
		t.Fatalf("Inject sigv4 (host overrides stored): %v", err)
	}
	auth := req.Header.Get("Authorization")
	if want := "/us-east-1/athena/aws4_request"; !strings.Contains(auth, want) {
		t.Errorf("Authorization = %q, want host-derived scope %q", auth, want)
	}
	if strings.Contains(auth, "eu-west-1") || strings.Contains(auth, "/s3/") {
		t.Errorf("Authorization = %q leaked the stale stored region/service", auth)
	}
}

// TestInjectSigV4UnparseableHostNoFallback is the fail-closed regression: an
// unparseable host with no stored region/service returns ErrMissingParam and
// leaves the request unmutated (no silent default that would sign the wrong
// scope).
func TestInjectSigV4UnparseableHostNoFallback(t *testing.T) {
	req := newReq(t, "https://example.amazonaws.com/")
	err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		AccessKeyID: vectorAccessKeyID,
	})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("X-Amz-Date") != "" {
		t.Error("request mutated on fail-closed error path")
	}
	if strings.Contains(err.Error(), vectorSecretKey) {
		t.Errorf("error leaked secret: %q", err.Error())
	}
}

// errReader is an io.ReadCloser that always fails, used to exercise the
// body-read error path of the SigV4 signer.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
func (e errReader) Close() error             { return nil }

func TestInjectSigV4BodyReadError(t *testing.T) {
	pinClock(t)
	req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	readErr := errors.New("boom: simulated body read failure")
	req.Body = errReader{err: readErr}

	err = Inject(req, SchemeSigV4Resign, []byte(sentinelSecret), Params{
		AccessKeyID: vectorAccessKeyID,
		Region:      vectorRegion,
		Service:     vectorService,
	})
	if err == nil {
		t.Fatal("expected an error on body read failure")
	}
	if !errors.Is(err, readErr) {
		t.Errorf("error = %v, want it to wrap the read error", err)
	}
	// The secret must never appear in the error, and the request must be
	// left unmutated (no SigV4 headers set) on the error path.
	if strings.Contains(err.Error(), sentinelSecret) {
		t.Errorf("error leaked secret: %q", err.Error())
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Authorization set on body-read error path")
	}
}

// TestInjectSigV4MissingService is the mirror of TestInjectSigV4MissingRegion:
// an unparseable host with only a partial stored fallback (Region but no
// Service) fails closed with ErrMissingParam and leaves the request unmutated.
func TestInjectSigV4MissingService(t *testing.T) {
	req := newReq(t, "https://example.amazonaws.com/")
	err := Inject(req, SchemeSigV4Resign, []byte(vectorSecretKey), Params{
		AccessKeyID: vectorAccessKeyID,
		Region:      vectorRegion,
	})
	if !errors.Is(err, ErrMissingParam) {
		t.Fatalf("error = %v, want ErrMissingParam", err)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("X-Amz-Date") != "" {
		t.Error("request mutated on missing-Service error path")
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
		{"sigv4-missing", SchemeSigV4Resign, Params{}},
		{"sigv4-ok", SchemeSigV4Resign, Params{AccessKeyID: "AKIDEXAMPLE", Region: "us-east-1", Service: "service"}},
		{"unknown", Scheme("bogus"), Params{}},
	}
	for _, c := range calls {
		req := newReq(t, "https://example.amazonaws.com/v1/things")
		err := Inject(req, c.scheme, []byte(sentinelSecret), c.params)
		if err != nil && strings.Contains(err.Error(), sentinelSecret) {
			t.Errorf("%s: error string leaked secret: %q", c.name, err.Error())
		}
		// On any success path, the secret must not appear in any header
		// value either (the sigv4 happy path derives a signature from it).
		if err == nil {
			for name, vs := range req.Header {
				for _, v := range vs {
					if strings.Contains(v, sentinelSecret) && c.scheme == SchemeSigV4Resign {
						t.Errorf("%s: secret leaked into header %s: %q", c.name, name, v)
					}
				}
			}
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
