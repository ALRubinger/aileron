// Package main is the test-only fake binary the runtime's
// integration tests wrap to prove the platform sandbox enforces
// the spawn-primitive threat model. The same source is compiled
// and invoked on every CI runner (linux, macOS, Windows), so the
// assertions across the matrix exercise identical attack
// scenarios and the test surface stays portable.
//
// The binary takes a `--scenario` flag plus scenario-specific
// arguments. Each scenario performs ONE forbidden action and
// reports the outcome via exit code:
//
//   - exit 0   the forbidden action succeeded (sandbox FAILED to block).
//   - exit 1   the action was blocked by the kernel (sandbox enforced).
//   - exit 2   the scenario itself was misused (malformed flags etc.).
//
// Stderr carries the underlying error for diagnostic purposes.
//
// Intentionally minimal: no third-party imports, no Go-runtime
// surprises that could mask whether the kernel did its job. The
// binary's behavior is the test subject; the wrapping runtime is
// the system under test.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	scenario := flag.String("scenario", "", "scenario: read")
	path := flag.String("path", "", "path operand (read: file to open)")
	flag.Parse()

	switch *scenario {
	case "read":
		if *path == "" {
			misuse("read requires --path")
		}
		runRead(*path)
	default:
		misuse("unknown or missing --scenario: %q", *scenario)
	}
}

// runRead attempts to read the file at `path` and prints a short
// summary on success. Exits 0 when the read succeeded (the kernel
// allowed it), exits 1 when it failed (the kernel blocked it).
// The test treats a non-zero exit as evidence the sandbox
// enforced the manifest's fs_read declaration.
func runRead(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakebinary: read %s: %v\n", path, err)
		os.Exit(1)
	}
	n := len(data)
	if n > 32 {
		n = 32
	}
	// Truncated preview avoids dumping potentially-sensitive
	// contents in test logs while still giving the test enough
	// signal to verify the read succeeded.
	fmt.Printf("fakebinary: read %d bytes from %s; preview=%q\n", len(data), path, string(data[:n]))
	os.Exit(0)
}

// misuse is the exit path for malformed scenario invocations.
// Distinguished from a normal block by exit code 2 so the test
// can tell "you misused the fixture" apart from "the kernel
// blocked the action."
func misuse(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fakebinary: "+format+"\n", args...)
	os.Exit(2)
}
