// Command scenario is the shell-free Go equivalent of test/system/codex.sh: it
// drives a live `aileron launch <agent>` against a real Docker sandbox and runs
// the R7a/R8.1-8.6/R9/R10 assertions, delegating every decision to the pure
// test/system/lib (systestlib) package.
//
// It is invoked BY HAND on a real, agent-authed host (it performs a live launch
// that needs a real agent login and consumes LLM tokens), exactly like the bash
// scenario it replaces:
//
//	AILERON_BIN=/abs/path/to/aileron \
//	WORKSPACE=/tmp/aileron-systest \
//	AILERON_STATE_DIR=$HOME/.aileron \
//	  go run ./test/system/scenario codex
//
// This package is its own nested Go module OUTSIDE ./test/system/lib/..., so the
// standard `go test ./test/system/lib/...` coverage/vet CI job never compiles or
// executes it and cannot start a real container. Its pure logic is covered by the
// systestlib unit tests; this package holds only the impure docker/exec/poll
// plumbing. There are intentionally no tests here.
//
// Required environment (the bash target exported these; the AILERON_SYSTEST_LIB
// var the bash needed is now vestigial because the lib is an imported package):
//
//	AILERON_BIN          absolute path to the freshly built aileron binary
//	WORKSPACE            temp workspace dir bind-mounted into the container
//	AILERON_STATE_DIR    daemon state dir for the R10 audit read (optional)
//	EXPECTED_IMAGE       override the default expected image ref (optional)
//
// Exit 0 means every assertion passed; a non-zero exit aborts (and reaps the
// background launch so its `docker run --rm` container is torn down).
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "systest: FAIL: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches on the agent name argument. Today only codex is wired (#1624);
// claude lands in #1625 reusing the same runner with its own AgentConfig
// constructor, so this dispatch grows one case, not a new driver.
func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: scenario <agent>  (supported: codex)")
	}
	agent := args[0]

	env, err := loadEnv()
	if err != nil {
		return err
	}

	switch agent {
	case "codex":
		return runCodex(env)
	default:
		return fmt.Errorf("unsupported agent %q (supported: codex)", agent)
	}
}
