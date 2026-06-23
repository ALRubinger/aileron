package nodedist_test

import (
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
)

func TestResolveURLs(t *testing.T) {
	cases := []struct {
		name      string
		version   string
		target    nodedist.Target
		archive   string
		checksums string
		signature string
		distro    string
	}{
		{
			name:      "linux amd64 tar.gz",
			version:   "24.2.0",
			target:    nodedist.Target{GOOS: "linux", GOARCH: "amd64"},
			archive:   "https://nodejs.org/dist/v24.2.0/node-v24.2.0-linux-x64.tar.gz",
			checksums: "https://nodejs.org/dist/v24.2.0/SHASUMS256.txt",
			signature: "https://nodejs.org/dist/v24.2.0/SHASUMS256.txt.asc",
			distro:    "node-v24.2.0-linux-x64",
		},
		{
			name:    "darwin arm64 tar.gz",
			version: "24.2.0",
			target:  nodedist.Target{GOOS: "darwin", GOARCH: "arm64"},
			archive: "https://nodejs.org/dist/v24.2.0/node-v24.2.0-darwin-arm64.tar.gz",
			distro:  "node-v24.2.0-darwin-arm64",
		},
		{
			name:    "windows amd64 zip",
			version: "24.2.0",
			target:  nodedist.Target{GOOS: "windows", GOARCH: "amd64"},
			archive: "https://nodejs.org/dist/v24.2.0/node-v24.2.0-win-x64.zip",
			distro:  "node-v24.2.0-win-x64",
		},
		{
			name:    "leading v in version is accepted",
			version: "v20.11.1",
			target:  nodedist.Target{GOOS: "linux", GOARCH: "arm64"},
			archive: "https://nodejs.org/dist/v20.11.1/node-v20.11.1-linux-arm64.tar.gz",
			distro:  "node-v20.11.1-linux-arm64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			urls, err := nodedist.ResolveURLs(tc.version, tc.target)
			if err != nil {
				t.Fatalf("ResolveURLs: %v", err)
			}
			if urls.Archive != tc.archive {
				t.Errorf("Archive = %q, want %q", urls.Archive, tc.archive)
			}
			if tc.checksums != "" && urls.Checksums != tc.checksums {
				t.Errorf("Checksums = %q, want %q", urls.Checksums, tc.checksums)
			}
			if tc.signature != "" && urls.Signature != tc.signature {
				t.Errorf("Signature = %q, want %q", urls.Signature, tc.signature)
			}

			distro, err := nodedist.DistroName(tc.version, tc.target)
			if err != nil {
				t.Fatalf("DistroName: %v", err)
			}
			if distro != tc.distro {
				t.Errorf("DistroName = %q, want %q", distro, tc.distro)
			}
		})
	}
}

func TestResolveURLs_UnsupportedTarget(t *testing.T) {
	_, err := nodedist.ResolveURLs("24.2.0", nodedist.Target{GOOS: "plan9", GOARCH: "mips"})
	if err == nil {
		t.Fatalf("ResolveURLs accepted an unsupported target")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrUnsupportedTarget {
		t.Fatalf("error = %v, want ErrUnsupportedTarget", err)
	}
}

func TestResolveURLs_EmptyVersion(t *testing.T) {
	_, err := nodedist.ResolveURLs("", nodedist.Target{GOOS: "linux", GOARCH: "amd64"})
	if err == nil {
		t.Fatalf("ResolveURLs accepted an empty version")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrInvalidVersion {
		t.Fatalf("error = %v, want ErrInvalidVersion", err)
	}
}

func TestArchiveName_WindowsZip(t *testing.T) {
	name, err := nodedist.ArchiveName("24.2.0", nodedist.Target{GOOS: "windows", GOARCH: "arm64"})
	if err != nil {
		t.Fatalf("ArchiveName: %v", err)
	}
	if name != "node-v24.2.0-win-arm64.zip" {
		t.Fatalf("ArchiveName = %q", name)
	}
}
