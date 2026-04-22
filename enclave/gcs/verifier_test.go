package gcs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// buildTestJWT creates a signed JWT for testing.
func buildTestJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()

	header := map[string]string{"alg": "RS256", "kid": kid}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	signingInput := headerB64 + "." + claimsB64
	h := crypto.SHA256.New()
	h.Write([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		t.Fatalf("signing JWT: %v", err)
	}
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sigB64
}

// setupTestServer creates test OIDC discovery and JWKS endpoints.
func setupTestServer(t *testing.T, key *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		baseURL := "http://" + r.Host
		json.NewEncoder(w).Encode(map[string]string{
			"jwks_uri": baseURL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{
				{
					"kty": "RSA",
					"kid": kid,
					"alg": "RS256",
					"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
				},
			},
		})
	})
	return httptest.NewServer(mux)
}

func TestVerifyValidToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	nonce := []byte("test-nonce-123")
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)

	now := time.Now()
	expTime := now.Add(time.Hour)
	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":          "https://accounts.google.com",
		"exp":          expTime.Unix(),
		"iat":          now.Unix(),
		"eat_nonce":    []string{nonceB64},
		"image_digest": "sha256:abc123",
		"project_id":   "my-project",
		"hwmodel":      "GCP_AMD_SEV",
	})

	v := &Verifier{
		ExpectedImageDigest: "sha256:abc123",
		ExpectedProjectID:   "my-project",
		DiscoveryURL:        server.URL + "/.well-known/openid-configuration",
	}

	claims, err := v.Verify(context.Background(), token, nonce)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.ImageDigest != "sha256:abc123" {
		t.Fatalf("expected sha256:abc123, got %q", claims.ImageDigest)
	}
	if claims.ProjectID != "my-project" {
		t.Fatalf("expected my-project, got %q", claims.ProjectID)
	}
	if claims.Issuer != "https://accounts.google.com" {
		t.Fatalf("expected issuer https://accounts.google.com, got %q", claims.Issuer)
	}
	if claims.HWModel != "GCP_AMD_SEV" {
		t.Fatalf("expected hwmodel GCP_AMD_SEV, got %q", claims.HWModel)
	}
	if claims.IssuedAt.Unix() != now.Unix() {
		t.Fatalf("expected IssuedAt %v, got %v", now.Unix(), claims.IssuedAt.Unix())
	}
	if claims.ExpiresAt.Unix() != expTime.Unix() {
		t.Fatalf("expected ExpiresAt %v, got %v", expTime.Unix(), claims.ExpiresAt.Unix())
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	nonce := []byte("nonce")
	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":       "https://accounts.google.com",
		"exp":       time.Now().Add(-time.Hour).Unix(),
		"eat_nonce": []string{base64.RawURLEncoding.EncodeToString(nonce)},
	})

	v := &Verifier{DiscoveryURL: server.URL + "/.well-known/openid-configuration"}
	_, err := v.Verify(context.Background(), token, nonce)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestVerifyWrongIssuer(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	nonce := []byte("nonce")
	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":       "https://evil.example.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"eat_nonce": []string{base64.RawURLEncoding.EncodeToString(nonce)},
	})

	v := &Verifier{DiscoveryURL: server.URL + "/.well-known/openid-configuration"}
	_, err := v.Verify(context.Background(), token, nonce)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestVerifyNonceMismatch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":       "https://accounts.google.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"eat_nonce": []string{"wrong-nonce"},
	})

	v := &Verifier{DiscoveryURL: server.URL + "/.well-known/openid-configuration"}
	_, err := v.Verify(context.Background(), token, []byte("correct-nonce"))
	if err == nil {
		t.Fatal("expected error for nonce mismatch")
	}
}

func TestVerifyImageDigestMismatch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	nonce := []byte("nonce")
	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":          "https://accounts.google.com",
		"exp":          time.Now().Add(time.Hour).Unix(),
		"eat_nonce":    []string{base64.RawURLEncoding.EncodeToString(nonce)},
		"image_digest": "sha256:wrong",
		"project_id":   "my-project",
	})

	v := &Verifier{
		ExpectedImageDigest: "sha256:correct",
		DiscoveryURL:        server.URL + "/.well-known/openid-configuration",
	}
	_, err := v.Verify(context.Background(), token, nonce)
	if err == nil {
		t.Fatal("expected error for image digest mismatch")
	}
}

func TestVerifyInvalidJWT(t *testing.T) {
	v := &Verifier{}
	_, err := v.Verify(context.Background(), "not-a-jwt", nil)
	if err == nil {
		t.Fatal("expected error for invalid JWT")
	}
}

func TestVerifyProjectIDMismatch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	nonce := []byte("nonce")
	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":          "https://accounts.google.com",
		"exp":          time.Now().Add(time.Hour).Unix(),
		"eat_nonce":    []string{base64.RawURLEncoding.EncodeToString(nonce)},
		"image_digest": "sha256:abc",
		"project_id":   "wrong-project",
	})

	v := &Verifier{
		ExpectedProjectID: "my-project",
		DiscoveryURL:      server.URL + "/.well-known/openid-configuration",
	}
	_, err := v.Verify(context.Background(), token, nonce)
	if err == nil {
		t.Fatal("expected error for project ID mismatch")
	}
}

func TestVerifyKeyNotFound(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	server := setupTestServer(t, &key.PublicKey, "server-key")
	defer server.Close()

	nonce := []byte("nonce")
	// Token uses kid "other-key" which doesn't exist in JWKS.
	token := buildTestJWT(t, key, "other-key", map[string]any{
		"iss":       "https://accounts.google.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"eat_nonce": []string{base64.RawURLEncoding.EncodeToString(nonce)},
	})

	v := &Verifier{DiscoveryURL: server.URL + "/.well-known/openid-configuration"}
	_, err := v.Verify(context.Background(), token, nonce)
	if err == nil {
		t.Fatal("expected error for key not found")
	}
}

func TestClientClose(t *testing.T) {
	c := New(Config{BaseURL: "http://localhost:9999"})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestVerifyWithNowFunc(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	nonce := []byte("nonce")
	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":       "https://accounts.google.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"eat_nonce": []string{base64.RawURLEncoding.EncodeToString(nonce)},
	})

	// Use NowFunc to simulate time far in the future (expired).
	v := &Verifier{
		DiscoveryURL: server.URL + "/.well-known/openid-configuration",
		NowFunc:      func() time.Time { return time.Now().Add(2 * time.Hour) },
	}
	_, err := v.Verify(context.Background(), token, nonce)
	if err == nil {
		t.Fatal("expected error for expired token with NowFunc")
	}
}

func TestVerifyWithCustomHTTPClient(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	server := setupTestServer(t, &key.PublicKey, kid)
	defer server.Close()

	nonce := []byte("nonce")
	token := buildTestJWT(t, key, kid, map[string]any{
		"iss":       "https://accounts.google.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"eat_nonce": []string{base64.RawURLEncoding.EncodeToString(nonce)},
	})

	v := &Verifier{
		DiscoveryURL: server.URL + "/.well-known/openid-configuration",
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	}
	_, err := v.Verify(context.Background(), token, nonce)
	if err != nil {
		t.Fatalf("Verify with custom client: %v", err)
	}
}

func TestVerifyES256Token(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating EC key: %v", err)
	}

	kid := "ec-key-1"
	// Setup JWKS with EC key.
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"jwks_uri": "http://" + r.Host + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{
				{
					"kty": "EC",
					"kid": kid,
					"alg": "ES256",
					"crv": "P-256",
					"x":   base64.RawURLEncoding.EncodeToString(ecKey.PublicKey.X.Bytes()),
					"y":   base64.RawURLEncoding.EncodeToString(ecKey.PublicKey.Y.Bytes()),
				},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	nonce := []byte("ec-nonce")
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)

	// Build ES256 JWT.
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": kid})
	claims, _ := json.Marshal(map[string]any{
		"iss":       "https://accounts.google.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"eat_nonce": []string{nonceB64},
	})
	headerB64 := base64.RawURLEncoding.EncodeToString(header)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claims)
	signingInput := headerB64 + "." + claimsB64

	h := crypto.SHA256.New()
	h.Write([]byte(signingInput))
	sig, err := ecdsa.SignASN1(rand.Reader, ecKey, h.Sum(nil))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	token := signingInput + "." + sigB64

	v := &Verifier{DiscoveryURL: server.URL + "/.well-known/openid-configuration"}
	_, err = v.Verify(context.Background(), token, nonce)
	if err != nil {
		t.Fatalf("Verify ES256: %v", err)
	}
}

func TestVerifyUnsupportedAlgorithm(t *testing.T) {
	err := verifySignature("RS384", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported algorithm")
	}
}

func TestToPublicKeyUnsupportedType(t *testing.T) {
	k := &jwk{Kty: "OKP"}
	_, err := k.toPublicKey()
	if err == nil {
		t.Fatal("expected error for unsupported key type")
	}
}

func TestToPublicKeyUnsupportedCurve(t *testing.T) {
	k := &jwk{Kty: "EC", Crv: "P-521", X: "AA", Y: "AA"}
	_, err := k.toPublicKey()
	if err == nil {
		t.Fatal("expected error for unsupported curve")
	}
}

func TestVerifyWrongSignature(t *testing.T) {
	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	key2, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"

	// JWKS serves key2, but token is signed with key1.
	server := setupTestServer(t, &key2.PublicKey, kid)
	defer server.Close()

	nonce := []byte("nonce")
	token := buildTestJWT(t, key1, kid, map[string]any{
		"iss":       "https://accounts.google.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"eat_nonce": []string{base64.RawURLEncoding.EncodeToString(nonce)},
	})

	v := &Verifier{DiscoveryURL: server.URL + "/.well-known/openid-configuration"}
	_, err := v.Verify(context.Background(), token, nonce)
	if err == nil {
		t.Fatal("expected error for wrong signature")
	}
}
