package inject

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// now is the package clock, indirected so tests can pin the signing time
// to a published-vector value. Production always uses [time.Now]. It is
// only assigned by in-package tests, never by callers, and the secret is
// never involved in this indirection.
var now = time.Now

// emptyPayloadHash is the hex SHA-256 of the empty string, the
// AWS-documented payload hash for a request with no body.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

const (
	sigV4Algorithm    = "AWS4-HMAC-SHA256"
	sigV4Terminator   = "aws4_request"
	amzDateFormat     = "20060102T150405Z"
	scopeDateFormat   = "20060102"
	hdrAmzDate        = "X-Amz-Date"
	hdrAmzContentHash = "X-Amz-Content-Sha256"
	hdrAuthorization  = "Authorization"
)

// signSigV4 computes an AWS Signature Version 4 over req using the
// std-library crypto primitives only (no AWS SDK) and mutates req to
// carry the three SigV4 headers (X-Amz-Date, X-Amz-Content-Sha256,
// Authorization) in that order.
//
// secretAccessKey is the AWS secret access key and is consumed only as
// HMAC key material to derive the signing key; it never appears in any
// header value, error, or other observable surface. accessKeyID, region,
// and service are non-secret and are validated by the caller. signingTime
// fixes the X-Amz-Date and credential-scope date.
//
// For a request with a body, the body is read fully, SHA-256 hashed for
// the payload hash, then restored (so the request stays sendable) by
// resetting req.Body, req.ContentLength, and req.GetBody. For a nil body
// the empty-payload hash is used. On a body-read error req is left
// unmutated and a wrapped error containing no secret material is returned.
func signSigV4(req *http.Request, secretAccessKey []byte, accessKeyID, region, service string, signingTime time.Time) error {
	t := signingTime.UTC()
	amzDate := t.Format(amzDateFormat)
	scopeDate := t.Format(scopeDateFormat)

	payloadHash, err := hashPayload(req)
	if err != nil {
		// Wrap without including any request/secret bytes.
		return fmt.Errorf("inject: sigv4-resign read request body: %w", err)
	}

	// Stage the headers that participate in the signature. We set them on
	// the request first so the canonical/signed header derivation reads
	// the real header set (which keeps a future X-Amz-Security-Token
	// threadable without re-architecting), then compute the signature, then
	// add Authorization. The only fallible step (reading the body) ran
	// above, so from here on signing is pure and req is fully mutated.
	req.Header.Set(hdrAmzDate, amzDate)
	req.Header.Set(hdrAmzContentHash, payloadHash)

	canonicalHeaders, signedHeaders := canonicalAndSignedHeaders(req)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req),
		canonicalQueryString(req),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{scopeDate, region, service, sigV4Terminator}, "/")

	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(secretAccessKey, scopeDate, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authorization := fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm, accessKeyID, scope, signedHeaders, signature,
	)
	req.Header.Set(hdrAuthorization, authorization)
	return nil
}

// hashPayload returns the hex SHA-256 of the request body, restoring the
// body so the request remains sendable. A nil body hashes to the empty
// payload hash.
func hashPayload(req *http.Request) (string, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return emptyPayloadHash, nil
	}
	buf, err := io.ReadAll(req.Body)
	if err != nil {
		return "", err
	}
	// io.ReadAll does not close; close the original to release resources.
	_ = req.Body.Close()

	req.Body = io.NopCloser(bytes.NewReader(buf))
	req.ContentLength = int64(len(buf))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	return hexSHA256(buf), nil
}

// canonicalURI returns the URI-encoded absolute path per the SigV4
// canonical-request rules. The path is split on "/" and each segment is
// RFC 3986 percent-encoded (the non-S3 single-encoding rule the published
// aws-sig-v4-test-suite expects). An empty path canonicalizes to "/".
func canonicalURI(req *http.Request) string {
	path := req.URL.EscapedPath()
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		// Decode any pre-existing escapes back to raw bytes, then re-encode
		// per the SigV4 single-encoding rule so the canonical form is
		// independent of how the caller happened to escape the path.
		segments[i] = uriEncode(percentDecode(seg), false)
	}
	return strings.Join(segments, "/")
}

func fromHex(a, b byte) (byte, bool) {
	hi, ok1 := hexNibble(a)
	lo, ok2 := hexNibble(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// canonicalQueryString returns the sorted, percent-encoded query string
// per the SigV4 rules: parameters sorted by name (then value), each name
// and value URI-encoded, joined by "&" with "=" between name and value.
func canonicalQueryString(req *http.Request) string {
	raw := req.URL.RawQuery
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, kv{
			k: uriEncode(percentDecode(name), true),
			v: uriEncode(percentDecode(value), true),
		})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

// percentDecode decodes %XX escapes in s back to raw bytes so uriEncode
// can re-encode consistently. '+' is left as-is (it is not query-space in
// the canonical-string rules).
func percentDecode(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if b, ok := fromHex(s[i+1], s[i+2]); ok {
				out = append(out, b)
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

// canonicalAndSignedHeaders derives the canonical-headers block and the
// signed-headers list from the request's actual header set. Header names
// are lowercased, values are trimmed and internal runs of spaces are
// collapsed, entries are sorted by name, and each is rendered as
// "name:value\n". The signed-headers list is the ";"-joined sorted names.
// Deriving from the real header set (rather than a hardcoded slice) is
// what keeps a future X-Amz-Security-Token threadable.
func canonicalAndSignedHeaders(req *http.Request) (canonical, signed string) {
	names := make([]string, 0, len(req.Header)+1)
	values := make(map[string]string, len(req.Header)+1)

	// host is signed but lives on req.Host / req.URL.Host, not in Header.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	names = append(names, "host")
	values["host"] = host

	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		joined := make([]string, len(vs))
		for i, v := range vs {
			joined[i] = trimHeaderValue(v)
		}
		values[lower] = strings.Join(joined, ",")
		names = append(names, lower)
	}
	sort.Strings(names)

	var cb strings.Builder
	for _, n := range names {
		cb.WriteString(n)
		cb.WriteByte(':')
		cb.WriteString(values[n])
		cb.WriteByte('\n')
	}
	return cb.String(), strings.Join(names, ";")
}

// trimHeaderValue trims surrounding whitespace and collapses internal runs
// of spaces to a single space, per the SigV4 canonical-header rule.
func trimHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "  ") {
		return v
	}
	return strings.Join(strings.Fields(v), " ")
}

// deriveSigningKey computes the SigV4 signing key:
// HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request").
func deriveSigningKey(secret []byte, date, region, service string) []byte {
	kDate := hmacSHA256(append([]byte("AWS4"), secret...), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(sigV4Terminator))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// uriEncode percent-encodes s per RFC 3986. Unreserved characters
// (A-Z a-z 0-9 - _ . ~) are passed through. When encodeSlash is false,
// "/" is also passed through (used for path segments joined by "/"); when
// true, "/" is encoded (used for query components).
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex(c >> 4))
			b.WriteByte(upperHex(c & 0x0f))
		}
	}
	return b.String()
}

func upperHex(n byte) byte {
	const hexDigits = "0123456789ABCDEF"
	return hexDigits[n]
}
