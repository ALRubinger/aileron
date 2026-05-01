// Package main is the test-only WASM connector fixture for the
// end-to-end install + execute pipeline (#366).
//
// Build with:
//
//	GOOS=wasip1 GOARCH=wasm go build -o ../github-prs-test.wasm .
//
// This fixture is intentionally narrow — it exercises the credential
// mediation path against any HTTP upstream (the integration test
// stands up an httptest.NewServer impersonating GitHub). The real
// `aileron-connector-github` connector lives in its own repo with its
// own versioning and release lifecycle (per ADR-0002); this fixture
// is the test stand-in that proves the framework wiring works.
//
// Op:
//
//	"list_prs"  args { url: string } — calls the supplied URL with
//	            credential: "api_key" attached. The action template
//	            controls the URL via ${args.url} interpolation, so the
//	            integration test passes the test server's URL as the
//	            top-level args.
//
//go:build wasip1

package main

import (
	"encoding/json"
	"io"
	"os"
	"unsafe"
)

//go:wasmimport aileron_host log
//go:noescape
func hostLog(levelPtr unsafe.Pointer, levelLen uint32, msgPtr unsafe.Pointer, msgLen uint32)

//go:wasmimport aileron_host http_request
//go:noescape
func hostHTTPRequest(reqPtr unsafe.Pointer, reqLen uint32) int32

//go:wasmimport aileron_host http_response_size
//go:noescape
func hostHTTPResponseSize() int32

//go:wasmimport aileron_host http_response_status
//go:noescape
func hostHTTPResponseStatus() int32

//go:wasmimport aileron_host http_response_read
//go:noescape
func hostHTTPResponseRead(dstPtr unsafe.Pointer, dstLen uint32) int32

func ptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return unsafe.Pointer(&_emptyPtrSentinel[0])
	}
	return unsafe.Pointer(&b[0])
}

var _emptyPtrSentinel = [1]byte{}

type input struct {
	Op   string         `json:"op"`
	Args map[string]any `json:"args"`
}

type output struct {
	Output map[string]any `json:"output,omitempty"`
	Error  *outputError   `json:"error,omitempty"`
}

type outputError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeError("read_stdin", err.Error())
		os.Exit(1)
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		writeError("parse_input", err.Error())
		os.Exit(1)
	}

	switch in.Op {
	case "list_prs":
		url, _ := in.Args["url"].(string)
		// credential_kind in args lets the action template choose
		// which kind the host should resolve. Defaults to api_key
		// for back-compat with existing fixtures.
		credKind, _ := in.Args["credential_kind"].(string)
		if credKind == "" {
			credKind = "api_key"
		}
		req, _ := json.Marshal(map[string]any{
			"method":     "GET",
			"url":        url,
			"credential": credKind,
			"headers":    map[string]string{"Accept": "application/json"},
		})
		rc := hostHTTPRequest(ptr(req), uint32(len(req)))
		if rc != 0 {
			writeOutput(map[string]any{"http_request_rc": int(rc)})
			return
		}
		size := hostHTTPResponseSize()
		status := hostHTTPResponseStatus()
		out := map[string]any{
			"status": int(status),
			"size":   int(size),
		}
		if size > 0 {
			body := make([]byte, size)
			n := hostHTTPResponseRead(ptr(body), uint32(size))
			if n > 0 {
				out["body"] = string(body[:n])
			}
		}
		writeOutput(out)

	default:
		writeError("unknown_op", in.Op)
	}
}

func writeOutput(out map[string]any) {
	_ = json.NewEncoder(os.Stdout).Encode(output{Output: out})
}

func writeError(class, message string) {
	_ = json.NewEncoder(os.Stdout).Encode(output{Error: &outputError{Class: class, Message: message}})
}
