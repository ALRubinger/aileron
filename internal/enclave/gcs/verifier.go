package gcs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
)

// Verifier validates Google Confidential Space OIDC attestation tokens.
type Verifier struct {
	// ExpectedImageDigest is the required container image digest (sha256:...).
	ExpectedImageDigest string
	// ExpectedProjectID is the required GCP project ID.
	ExpectedProjectID string
	// HTTPClient is used to fetch the OIDC discovery document and JWKS.
	// If nil, http.DefaultClient is used.
	HTTPClient *http.Client
	// DiscoveryURL overrides the Google OIDC discovery endpoint (for testing).
	DiscoveryURL string
	// NowFunc overrides time.Now (for testing).
	NowFunc func() time.Time
}

const defaultDiscoveryURL = "https://accounts.google.com/.well-known/openid-configuration"

// Verify validates a Confidential Space attestation OIDC token.
func (v *Verifier) Verify(ctx context.Context, token string, nonce []byte) (enclave.AttestationClaims, error) {
	// Parse JWT without verification first to extract header.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return enclave.AttestationClaims{}, errors.New("gcs: invalid JWT format")
	}

	// Decode header to get key ID.
	headerBytes, err := base64URLDecode(parts[0])
	if err != nil {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: decoding JWT header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: parsing JWT header: %w", err)
	}

	// Fetch JWKS.
	jwksURL, err := v.fetchJWKSURL(ctx)
	if err != nil {
		return enclave.AttestationClaims{}, err
	}
	pubKey, err := v.fetchKey(ctx, jwksURL, header.Kid)
	if err != nil {
		return enclave.AttestationClaims{}, err
	}

	// Verify signature.
	signingInput := parts[0] + "." + parts[1]
	signature, err := base64URLDecode(parts[2])
	if err != nil {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: decoding JWT signature: %w", err)
	}
	if err := verifySignature(header.Alg, pubKey, []byte(signingInput), signature); err != nil {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: signature verification failed: %w", err)
	}

	// Decode and validate claims.
	claimsBytes, err := base64URLDecode(parts[1])
	if err != nil {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: decoding JWT claims: %w", err)
	}
	var claims struct {
		Iss          string   `json:"iss"`
		Exp          int64    `json:"exp"`
		Iat          int64    `json:"iat"`
		EatNonce     []string `json:"eat_nonce"`
		ImageDigest  string   `json:"image_digest"`
		ProjectID    string   `json:"project_id"`
		HWModel      string   `json:"hwmodel"`
		SwName       string   `json:"swname"`
	}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: parsing JWT claims: %w", err)
	}

	now := time.Now()
	if v.NowFunc != nil {
		now = v.NowFunc()
	}

	// Validate issuer.
	if claims.Iss != "https://accounts.google.com" {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: unexpected issuer %q", claims.Iss)
	}

	// Validate expiry.
	if now.Unix() > claims.Exp {
		return enclave.AttestationClaims{}, errors.New("gcs: token expired")
	}

	// Validate nonce.
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)
	nonceFound := false
	for _, n := range claims.EatNonce {
		if n == nonceB64 || n == string(nonce) {
			nonceFound = true
			break
		}
	}
	if !nonceFound {
		return enclave.AttestationClaims{}, errors.New("gcs: nonce mismatch")
	}

	// Validate workload identity.
	if v.ExpectedImageDigest != "" && claims.ImageDigest != v.ExpectedImageDigest {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: image digest mismatch: got %q, want %q", claims.ImageDigest, v.ExpectedImageDigest)
	}
	if v.ExpectedProjectID != "" && claims.ProjectID != v.ExpectedProjectID {
		return enclave.AttestationClaims{}, fmt.Errorf("gcs: project ID mismatch: got %q, want %q", claims.ProjectID, v.ExpectedProjectID)
	}

	return enclave.AttestationClaims{
		ImageDigest: claims.ImageDigest,
		ProjectID:   claims.ProjectID,
		Issuer:      claims.Iss,
		HWModel:     claims.HWModel,
		IssuedAt:    time.Unix(claims.Iat, 0),
		ExpiresAt:   time.Unix(claims.Exp, 0),
		Nonces:      claims.EatNonce,
	}, nil
}

func (v *Verifier) httpClient() *http.Client {
	if v.HTTPClient != nil {
		return v.HTTPClient
	}
	return http.DefaultClient
}

func (v *Verifier) discoveryURL() string {
	if v.DiscoveryURL != "" {
		return v.DiscoveryURL
	}
	return defaultDiscoveryURL
}

func (v *Verifier) fetchJWKSURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.discoveryURL(), nil)
	if err != nil {
		return "", err
	}
	resp, err := v.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("gcs: fetching OIDC discovery: %w", err)
	}
	defer resp.Body.Close()

	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("gcs: parsing OIDC discovery: %w", err)
	}
	return doc.JWKSURI, nil
}

func (v *Verifier) fetchKey(ctx context.Context, jwksURL, kid string) (crypto.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcs: fetching JWKS: %w", err)
	}
	defer resp.Body.Close()

	var jwks struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("gcs: parsing JWKS: %w", err)
	}

	for _, k := range jwks.Keys {
		if k.Kid == kid {
			return k.toPublicKey()
		}
	}
	return nil, fmt.Errorf("gcs: key %q not found in JWKS", kid)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	// RSA fields
	N string `json:"n"`
	E string `json:"e"`
	// EC fields
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k *jwk) toPublicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nBytes, err := base64URLDecode(k.N)
		if err != nil {
			return nil, fmt.Errorf("decoding RSA n: %w", err)
		}
		eBytes, err := base64URLDecode(k.E)
		if err != nil {
			return nil, fmt.Errorf("decoding RSA e: %w", err)
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: int(new(big.Int).SetBytes(eBytes).Int64()),
		}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		default:
			return nil, fmt.Errorf("unsupported EC curve %q", k.Crv)
		}
		xBytes, err := base64URLDecode(k.X)
		if err != nil {
			return nil, fmt.Errorf("decoding EC x: %w", err)
		}
		yBytes, err := base64URLDecode(k.Y)
		if err != nil {
			return nil, fmt.Errorf("decoding EC y: %w", err)
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

func verifySignature(alg string, key crypto.PublicKey, signingInput, signature []byte) error {
	switch alg {
	case "RS256":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("expected RSA public key")
		}
		h := crypto.SHA256.New()
		h.Write(signingInput)
		return rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, h.Sum(nil), signature)
	case "ES256":
		ecKey, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("expected ECDSA public key")
		}
		h := crypto.SHA256.New()
		h.Write(signingInput)
		if !ecdsa.VerifyASN1(ecKey, h.Sum(nil), signature) {
			return errors.New("ECDSA signature verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported algorithm %q", alg)
	}
}

func base64URLDecode(s string) ([]byte, error) {
	// Add padding if needed.
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}
