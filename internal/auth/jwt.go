package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the JWT claims issued by Aileron after authentication.
type Claims struct {
	jwt.RegisteredClaims
	EnterpriseID string `json:"ent_id"`
	Email        string `json:"email"`
	Role         string `json:"role"`
}

// TokenIssuer creates and validates JWTs.
type TokenIssuer struct {
	signingKey []byte
	issuer     string
	accessTTL  time.Duration
}

// NewTokenIssuer creates a token issuer with the given HMAC signing key.
func NewTokenIssuer(signingKey []byte, issuer string, accessTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{
		signingKey: signingKey,
		issuer:     issuer,
		accessTTL:  accessTTL,
	}
}

// Issue creates a signed JWT for the given user.
func (ti *TokenIssuer) Issue(userID, enterpriseID, email, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    ti.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ti.accessTTL)),
		},
		EnterpriseID: enterpriseID,
		Email:        email,
		Role:         role,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(ti.signingKey)
}

// Validate parses and validates a JWT, returning the claims.
func (ti *TokenIssuer) Validate(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return ti.signingKey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	return claims, nil
}

// GenerateRefreshToken creates a cryptographically random refresh token
// and returns both the raw token (to send to the client) and its SHA-256
// hash (to store in the database).
func GenerateRefreshToken() (raw string, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generating refresh token: %w", err)
	}
	raw = hex.EncodeToString(b)
	hash = HashToken(raw)
	return raw, hash, nil
}

// HashToken returns the hex-encoded SHA-256 hash of a token string.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
