package enclave

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// RequestMAC returns a base64url-encoded HMAC over the request binding fields.
func RequestMAC(key []byte, method, path string, body []byte, sessionID, timestamp, nonce string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(requestMACPayload(method, path, body, sessionID, timestamp, nonce)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyRequestMAC checks that mac authenticates the request binding fields.
func VerifyRequestMAC(key []byte, method, path string, body []byte, sessionID, timestamp, nonce, mac string) bool {
	expected := RequestMAC(key, method, path, body, sessionID, timestamp, nonce)
	return hmac.Equal([]byte(mac), []byte(expected))
}

func requestMACPayload(method, path string, body []byte, sessionID, timestamp, nonce string) string {
	bodyHash := sha256.Sum256(body)
	return strings.Join([]string{
		method,
		path,
		sessionID,
		timestamp,
		nonce,
		base64.RawURLEncoding.EncodeToString(bodyHash[:]),
	}, "\n")
}
