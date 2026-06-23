package nodedist_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

// archiveEntry describes one file to place in a synthetic Node archive.
type archiveEntry struct {
	name string // archive path, including the top-level distro prefix
	body []byte
	mode int64 // unix mode bits; exec bit is asserted in tests
}

// buildTarGz builds a gzipped tar containing entries, including a top-level
// directory entry for distroPrefix so the unpack prefix-strip is exercised.
func buildTarGz(t *testing.T, distroPrefix string, entries []archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Top-level directory entry.
	if err := tw.WriteHeader(&tar.Header{
		Name:     distroPrefix + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}); err != nil {
		t.Fatalf("write dir header: %v", err)
	}
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Typeflag: tar.TypeReg,
			Mode:     mode,
			Size:     int64(len(e.body)),
		}); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("write body %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}
