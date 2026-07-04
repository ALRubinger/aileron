package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vault put user/<service> contract (#1822): the CLI verb replaces the raw
// curl PUT that base64-mis-decoded secrets. These tests pin:
//
//   - an interactive (no --from-file) user put reads the value at a hidden
//     prompt and base64-encodes it on the wire, so a raw secret that is
//     itself valid base64 round-trips to the exact bytes typed;
//   - --from-file stores file bytes verbatim to /vault/user/<service>/credentials;
//   - a malformed <service> is rejected before any HTTP call;
//   - an empty value (typed or from an empty file) is rejected before HTTP;
//   - locked / no-vault daemon statuses map to non-zero exits.
//
// fakeVaultServer and scriptPrompt live in the sibling vault test files.

// TestRunVaultPutUser_InteractiveRoundTripsBytesVerbatim is the core
// regression: a 40-char AWS secret access key is every-byte valid base64, so
// a raw write (the old curl PUT footgun) silently decodes it to garbage and
// later surfaces as AWS SignatureDoesNotMatch. Routing it through
// `vault put user/<service>` must base64-ENCODE it, so the daemon decodes
// back to the exact bytes the operator typed.
func TestRunVaultPutUser_InteractiveRoundTripsBytesVerbatim(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	defer scriptPrompt(secret)()

	var wireValue, gotMethod, gotPath string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		var raw struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		wireValue = raw.Value
		w.WriteHeader(http.StatusNoContent)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "user/athena"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodPut || gotPath != "/vault/user/athena/credentials" {
		t.Errorf("request = %s %s, want PUT /vault/user/athena/credentials", gotMethod, gotPath)
	}
	// The wire value must be base64 of the typed secret, NOT the raw secret.
	if wireValue == secret {
		t.Fatalf("wire value is the raw secret %q; it must be base64-encoded", secret)
	}
	decoded, err := base64.StdEncoding.DecodeString(wireValue)
	if err != nil {
		t.Fatalf("wire value %q is not valid base64: %v", wireValue, err)
	}
	if string(decoded) != secret {
		t.Errorf("decoded wire value = %q, want the exact typed bytes %q", decoded, secret)
	}
	if wireValue != base64.StdEncoding.EncodeToString([]byte(secret)) {
		t.Errorf("wire value = %q, want base64(%q)", wireValue, secret)
	}
	if !strings.Contains(stdout.String(), "Stored user/athena") {
		t.Errorf("stdout = %q, want it to confirm the store", stdout.String())
	}
}

// --from-file stores the file bytes verbatim (base64-encoded on the wire) at
// the user endpoint, the same daemon path vault delete/list use.
func TestRunVaultPutUser_FromFileRoundTrips(t *testing.T) {
	fileBytes := []byte("gho_rawtokenbytes")
	var received []byte
	var gotPath string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body agentCredentialsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		received = body.Value
		w.WriteHeader(http.StatusNoContent)
	})

	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	if err := os.WriteFile(f, fileBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "user/github", "--from-file", f}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPath != "/vault/user/github/credentials" {
		t.Errorf("path = %q, want /vault/user/github/credentials", gotPath)
	}
	if !bytes.Equal(received, fileBytes) {
		t.Errorf("server received %q, want %q (verbatim)", received, fileBytes)
	}
	if !strings.Contains(stdout.String(), "Stored user/github") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// A malformed <service> (uppercase, whitespace, empty, leading dash, or an
// extra path segment) is rejected client-side before any HTTP call and
// before any prompt, so a typo never reaches the daemon or blocks on stdin.
func TestRunVaultPut_RejectsMalformedUserService(t *testing.T) {
	for _, arg := range []string{"user/BAD", "user/has space", "user/", "user/-leading", "user/foo/bar"} {
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"put", arg}, strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("arg %q: exit = 0, want non-zero for malformed service", arg)
		}
		if called {
			t.Errorf("arg %q: HTTP issued for a rejected service", arg)
		}
	}
}

// An empty value — whether typed at the prompt or read from an empty file —
// is rejected before any HTTP call.
func TestRunVaultPutUser_EmptyValueRejectedNoHTTP(t *testing.T) {
	t.Run("interactive", func(t *testing.T) {
		defer scriptPrompt("")()
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"put", "user/github"}, strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("exit = 0, want non-zero for empty typed value")
		}
		if called {
			t.Errorf("HTTP issued for empty typed value")
		}
		if !strings.Contains(stderr.String(), "empty") {
			t.Errorf("stderr = %q, want an empty-value message", stderr.String())
		}
	})
	t.Run("empty-file", func(t *testing.T) {
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		dir := t.TempDir()
		f := filepath.Join(dir, "empty")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"put", "user/github", "--from-file", f}, strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("exit = 0, want non-zero for empty file")
		}
		if called {
			t.Errorf("HTTP issued for empty file")
		}
	})
}

// Locked and no-vault daemon statuses map to a non-zero exit, mirroring the
// agent branch.
func TestRunVaultPutUser_LockedAndNoVault(t *testing.T) {
	restore := scriptPrompt("a-secret", "a-secret")
	defer restore()
	for _, status := range []int{http.StatusLocked, http.StatusServiceUnavailable} {
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"put", "user/github"}, strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("status %d: exit = 0, want non-zero", status)
		}
	}
}

// The user-put daemon path is the write-side twin of the delete path, so a
// value typed at the prompt round-trips to the same endpoint vault delete
// targets. This pins the escaping helper directly.
func TestUserCredentialDaemonPath(t *testing.T) {
	if got := userCredentialDaemonPath("github"); got != "/vault/user/github/credentials" {
		t.Errorf("userCredentialDaemonPath(github) = %q, want /vault/user/github/credentials", got)
	}
}
