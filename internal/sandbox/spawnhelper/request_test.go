package spawnhelper

import (
	"strings"
	"testing"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	// Contract: a request encoded by the runtime and decoded by the
	// helper recovers every field. Round-trip is the load-bearing
	// property of the transport.
	req := Request{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "log", "--since=2026-01-01"},
		Env:     []string{"PATH=/usr/bin", "GIT_AUTHOR_NAME=alr"},
		Cwd:     "/Users/alr/code/aileron",
		FSRead:  []string{"/Users/alr/code/", "/etc/gitconfig"},
		FSWrite: []string{"/Users/alr/.cache/aileron/"},
	}
	encoded, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	decoded, err := DecodeRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if decoded.Program != req.Program {
		t.Errorf("Program = %q, want %q", decoded.Program, req.Program)
	}
	if !equalSlice(decoded.Argv, req.Argv) {
		t.Errorf("Argv = %v, want %v", decoded.Argv, req.Argv)
	}
	if !equalSlice(decoded.Env, req.Env) {
		t.Errorf("Env = %v, want %v", decoded.Env, req.Env)
	}
	if decoded.Cwd != req.Cwd {
		t.Errorf("Cwd = %q, want %q", decoded.Cwd, req.Cwd)
	}
	if !equalSlice(decoded.FSRead, req.FSRead) {
		t.Errorf("FSRead = %v, want %v", decoded.FSRead, req.FSRead)
	}
	if !equalSlice(decoded.FSWrite, req.FSWrite) {
		t.Errorf("FSWrite = %v, want %v", decoded.FSWrite, req.FSWrite)
	}
}

func TestEncodeRequest_RejectsMissingProgram(t *testing.T) {
	_, err := EncodeRequest(Request{Argv: []string{"x"}})
	if err == nil || !strings.Contains(err.Error(), "Program") {
		t.Errorf("expected Program-required error, got %v", err)
	}
}

func TestEncodeRequest_RejectsMissingArgv(t *testing.T) {
	_, err := EncodeRequest(Request{Program: "/bin/echo"})
	if err == nil || !strings.Contains(err.Error(), "Argv") {
		t.Errorf("expected Argv-required error, got %v", err)
	}
}

func TestDecodeRequest_RejectsBadBase64(t *testing.T) {
	_, err := DecodeRequest("!!not base64!!")
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("expected base64 decode error, got %v", err)
	}
}

func TestDecodeRequest_RejectsBadJSON(t *testing.T) {
	_, err := DecodeRequest("bm90IGpzb24=") // base64("not json")
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("expected unmarshal error, got %v", err)
	}
}

func TestDecodeRequest_RejectsMissingProgramAfterDecode(t *testing.T) {
	// Wire-format tampering: someone could base64-encode a JSON
	// blob that's syntactically valid but missing required fields.
	// The helper has to fail closed.
	encoded, err := EncodeRequest(Request{Program: "/bin/x", Argv: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	// Substitute the encoded blob with one that decodes to {} —
	// `{}` is base64 `e30=`.
	_, err = DecodeRequest("e30=")
	if err == nil || !strings.Contains(err.Error(), "Program") {
		t.Errorf("expected Program-required error after empty decode, got %v", err)
	}
	_ = encoded
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
