package freeze

import (
	"context"
	"testing"
)

// dummyResolver returns a resolver that yields a valid digest, so a test that
// exercises a later failure (composer, catalog resolution) never trips over
// base resolution.
func dummyResolver() DigestResolver {
	return DigestResolverFunc(func(context.Context, string) (string, error) { return fakeDigest, nil })
}

func TestNilAdapters_ErrorNotPanic(t *testing.T) {
	var dr DigestResolverFunc
	if _, err := dr.ResolveDigest(context.Background(), "x"); err == nil {
		t.Error("a nil DigestResolverFunc must return an error, not panic")
	}
	var fc FeatureComposerFunc
	if _, err := fc.ComposeDigest(context.Background(), "base@"+fakeDigest, []string{"f"}); err == nil {
		t.Error("a nil FeatureComposerFunc must return an error, not panic")
	}
}

func TestVerify_MalformedPublicKey(t *testing.T) {
	priv, _ := genSigningKey(t)
	sig, _, err := Sign(priv, []byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify([]byte("content"), sig, []byte("not a pem key")); err == nil {
		t.Error("a malformed public key must fail Verify")
	}
}
