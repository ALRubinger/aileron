package enclave

import "testing"

func TestRequestMACVerifiesBoundFields(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	body := []byte(`{"user_id":"user-1"}`)

	mac := RequestMAC(key, "POST", "/execute", body, "sess-1", "2026-04-26T08:00:00Z", "nonce-1")

	if !VerifyRequestMAC(key, "POST", "/execute", body, "sess-1", "2026-04-26T08:00:00Z", "nonce-1", mac) {
		t.Fatal("expected MAC to verify")
	}
	if VerifyRequestMAC(key, "POST", "/execute", []byte(`{"user_id":"user-2"}`), "sess-1", "2026-04-26T08:00:00Z", "nonce-1", mac) {
		t.Fatal("expected body tamper to fail")
	}
	if VerifyRequestMAC(key, "POST", "/escrow", body, "sess-1", "2026-04-26T08:00:00Z", "nonce-1", mac) {
		t.Fatal("expected path tamper to fail")
	}
	if VerifyRequestMAC(key, "POST", "/execute", body, "sess-2", "2026-04-26T08:00:00Z", "nonce-1", mac) {
		t.Fatal("expected session tamper to fail")
	}
	if VerifyRequestMAC(key, "POST", "/execute", body, "sess-1", "2026-04-26T08:00:00Z", "nonce-2", mac) {
		t.Fatal("expected nonce tamper to fail")
	}
}
