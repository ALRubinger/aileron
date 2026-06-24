package main

import (
	"os/exec"
	"strings"
)

// docker.go holds the thin `docker ps` / `docker inspect` / `docker exec`
// shellouts. Each returns the raw fact (trimmed stdout, or a success boolean for
// the test-style probes) that a systestlib decision predicate consumes. Nothing
// here makes a pass/fail decision; the decisions live in systestlib so they are
// unit-tested without Docker.

// dockerPSNames returns the `{{.Names}}` block for running containers whose name
// matches the prefix filter. Errors are swallowed to "" (matching the shell's
// `2>/dev/null`): a transient docker error during the discovery poll is
// indistinguishable from "no container yet", and the explicit timeout after the
// poll loop is what reports a genuine failure.
func dockerPSNames(prefix string) string {
	out, err := exec.Command("docker", "ps",
		"--filter", "name="+prefix,
		"--format", "{{.Names}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerPSAllNames returns the `{{.Names}}` block for ALL containers (running or
// not) matching the prefix, used by the R8.5 teardown check.
func dockerPSAllNames(prefix string) string {
	out, err := exec.Command("docker", "ps", "-a",
		"--filter", "name="+prefix,
		"--format", "{{.Names}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerInspect runs `docker inspect -f <format> <container>` and returns the
// trimmed stdout plus any error.
func dockerInspect(container, format string) (string, error) {
	out, err := exec.Command("docker", "inspect", "-f", format, container).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerExecOutput runs `docker exec <container> <args...>` and returns the
// trimmed stdout plus any error.
func dockerExecOutput(container string, args ...string) (string, error) {
	cmdArgs := append([]string{"exec", container}, args...)
	out, err := exec.Command("docker", cmdArgs...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerExecOK runs `docker exec <container> <args...>` and reports whether it
// exited zero, mirroring the shell's `if docker exec … test -x …` boolean probes.
func dockerExecOK(container string, args ...string) bool {
	cmdArgs := append([]string{"exec", container}, args...)
	return exec.Command("docker", cmdArgs...).Run() == nil
}
