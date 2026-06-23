// Command toolslock regenerates the committed managed-toolchain lockfile
// (internal/sandbox/container/tools.lock.json). It is run by
// `task generate:tools-lock` when the pinned Node.js version changes.
//
// For the pinned Node version it resolves the per-platform sha256 of each
// supported distribution archive from Node's published SHASUMS256.txt and writes
// the platform->sha256 map. The managed-toolchain build (#1525) embeds this lock
// and the fetcher verifies a downloaded archive against it, so a build is
// reproducible and a tampered or drifted distribution is rejected.
//
// This regenerator does NOT verify the GPG signature on SHASUMS256.txt; that
// signature-verified fetch pipeline is a separate concern (#1525, sub-issue 1).
// Here the determinism and canonical rendering of the lock are what matter: a
// test asserts the committed file equals FormatToolsLock of the parsed lock.
//
// Usage:
//
//	go run ./tools/toolslock -version 22.14.0 -out sandbox/container/tools.lock.json
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/sandbox/container"
)

// supportedPlatforms maps each Node distribution platform token the managed
// toolchain pins to the archive filename suffix it appears under in
// SHASUMS256.txt. Windows ships a .zip; the unix platforms ship a .tar.gz.
var supportedPlatforms = map[string]string{
	"darwin-arm64": "tar.gz",
	"darwin-x64":   "tar.gz",
	"linux-arm64":  "tar.gz",
	"linux-x64":    "tar.gz",
	"win-x64":      "zip",
}

func main() {
	version := flag.String("version", "", "Node.js version to pin (e.g. 22.14.0), without a leading v")
	out := flag.String("out", "sandbox/container/tools.lock.json", "lockfile path to write")
	flag.Parse()

	v := strings.TrimPrefix(strings.TrimSpace(*version), "v")
	if v == "" {
		fmt.Fprintln(os.Stderr, "toolslock: -version is required (e.g. -version 22.14.0)")
		os.Exit(1)
	}

	sums, err := fetchSHASUMS(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch SHASUMS256 for node v%s: %v\n", v, err)
		os.Exit(1)
	}

	checksums := make(map[string]string, len(supportedPlatforms))
	for token, ext := range supportedPlatforms {
		file := fmt.Sprintf("node-v%s-%s.%s", v, token, ext)
		sum, ok := sums[file]
		if !ok {
			fmt.Fprintf(os.Stderr, "node v%s: no checksum for %s in SHASUMS256.txt\n", v, file)
			os.Exit(1)
		}
		checksums[token] = sum
		fmt.Fprintf(os.Stderr, "%s -> %s\n", token, sum)
	}

	data, err := container.FormatToolsLock(v, checksums)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render lockfile: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote node v%s with %d platform pin(s) to %s\n", v, len(checksums), *out)
}

// fetchSHASUMS downloads Node's SHASUMS256.txt for the given version and returns
// a filename->sha256 map. Each line is "<hex sha256>  <filename>". This is the
// bare hash list; it does NOT verify the detached GPG signature (sub-issue 1).
func fetchSHASUMS(version string) (map[string]string, error) {
	url := fmt.Sprintf("https://nodejs.org/dist/v%s/SHASUMS256.txt", version)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}

	sums := make(map[string]string)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		sums[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("SHASUMS256.txt for v%s was empty", version)
	}
	return sums, nil
}
