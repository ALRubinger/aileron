package nodedist

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const testDistro = "node-v24.2.0-linux-x64"

func tarGz(t *testing.T, write func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestUnpackTarGz_LayoutAndExecBit(t *testing.T) {
	data := tarGz(t, func(tw *tar.Writer) {
		writeTarDir(t, tw, testDistro+"/")
		writeTarDir(t, tw, testDistro+"/bin/")
		writeTarFile(t, tw, testDistro+"/bin/node", []byte("#!node"), 0o755)
		writeTarFile(t, tw, testDistro+"/README.md", []byte("readme"), 0o644)
	})

	dest := t.TempDir()
	if err := unpackArchive(data, "tar.gz", testDistro, dest); err != nil {
		t.Fatalf("unpackArchive: %v", err)
	}

	// Top-level prefix stripped: bin/node sits directly under dest.
	nodePath := filepath.Join(dest, "bin", "node")
	got, err := os.ReadFile(nodePath)
	if err != nil {
		t.Fatalf("read bin/node: %v", err)
	}
	if string(got) != "#!node" {
		t.Fatalf("bin/node content = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(nodePath)
		if err != nil {
			t.Fatalf("stat bin/node: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("bin/node lost its executable bit: %v", info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("README.md not unpacked: %v", err)
	}
}

// TestUnpackTarGz_DoesNotEscapeViaTraversal proves the contract: no archive
// entry, however its name is crafted, can write a file outside the unpack
// target. Entries whose names traverse out of the distro prefix are dropped;
// entries whose cleaned path would still escape are rejected. Either way,
// nothing lands next to dest.
func TestUnpackTarGz_DoesNotEscapeViaTraversal(t *testing.T) {
	data := tarGz(t, func(tw *tar.Writer) {
		writeTarDir(t, tw, testDistro+"/")
		writeTarFile(t, tw, testDistro+"/../escape.txt", []byte("pwned"), 0o644)
		writeTarFile(t, tw, testDistro+"/sub/../../escape2.txt", []byte("pwned"), 0o644)
		writeTarFile(t, tw, testDistro+"/bin/node", []byte("ok"), 0o755)
	})

	dest := t.TempDir()
	if err := unpackArchive(data, "tar.gz", testDistro, dest); err != nil {
		t.Fatalf("unpackArchive: %v", err)
	}
	// Legitimate entry still landed.
	if _, err := os.Stat(filepath.Join(dest, "bin", "node")); err != nil {
		t.Fatalf("legitimate entry not unpacked: %v", err)
	}
	// Nothing escaped to the parent of dest.
	parent := filepath.Dir(dest)
	for _, name := range []string{"escape.txt", "escape2.txt"} {
		if _, err := os.Stat(filepath.Join(parent, name)); err == nil {
			t.Fatalf("traversal entry %q wrote a file outside the unpack target", name)
		}
	}
}

// TestSafeJoin_RejectsTraversal exercises the path-traversal guard directly:
// any relative path that resolves outside dest is rejected with ErrUnpack.
func TestSafeJoin_RejectsTraversal(t *testing.T) {
	dest := t.TempDir()
	for _, rel := range []string{"../escape", "a/../../escape", "../../../../etc/passwd"} {
		_, err := safeJoin(dest, rel)
		if err == nil {
			t.Fatalf("safeJoin(%q) accepted an escaping path", rel)
		}
		var ndErr *Error
		if !errors.As(err, &ndErr) || ndErr.Kind != ErrUnpack {
			t.Fatalf("safeJoin(%q) error = %v, want ErrUnpack", rel, err)
		}
	}
	// A legitimate nested path is accepted.
	got, err := safeJoin(dest, "bin/node")
	if err != nil {
		t.Fatalf("safeJoin legit: %v", err)
	}
	if got != filepath.Join(dest, "bin", "node") {
		t.Fatalf("safeJoin legit = %q", got)
	}
}

func TestUnpackTarGz_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	data := tarGz(t, func(tw *tar.Writer) {
		writeTarDir(t, tw, testDistro+"/")
		if err := tw.WriteHeader(&tar.Header{
			Name:     testDistro + "/evil",
			Typeflag: tar.TypeSymlink,
			Linkname: "../../etc/passwd",
			Mode:     0o777,
		}); err != nil {
			t.Fatalf("write symlink header: %v", err)
		}
	})
	dest := t.TempDir()
	err := unpackArchive(data, "tar.gz", testDistro, dest)
	if err == nil {
		t.Fatalf("unpackArchive accepted an escaping symlink")
	}
	var ndErr *Error
	if !errors.As(err, &ndErr) || ndErr.Kind != ErrUnpack {
		t.Fatalf("error = %v, want ErrUnpack", err)
	}
}

func TestUnpackZip_LayoutAndExecBit(t *testing.T) {
	winDistro := "node-v24.2.0-win-x64"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipFile(t, zw, winDistro+"/node.exe", []byte("MZexe"), 0o755)
	writeZipFile(t, zw, winDistro+"/npm.cmd", []byte("@echo"), 0o644)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	dest := t.TempDir()
	if err := unpackArchive(buf.Bytes(), "zip", winDistro, dest); err != nil {
		t.Fatalf("unpackArchive zip: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "node.exe"))
	if err != nil {
		t.Fatalf("read node.exe: %v", err)
	}
	if string(got) != "MZexe" {
		t.Fatalf("node.exe content = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dest, "node.exe"))
		if err != nil {
			t.Fatalf("stat node.exe: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("node.exe lost exec bit: %v", info.Mode())
		}
	}
}

func TestUnpackZip_DoesNotEscapeViaTraversal(t *testing.T) {
	winDistro := "node-v24.2.0-win-x64"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// CreateHeader does not sanitize names; craft traversal entries.
	for _, name := range []string{winDistro + "/../escape.txt", winDistro + "/a/../../escape2.txt"} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatalf("create header %s: %v", name, err)
		}
		if _, err := w.Write([]byte("pwned")); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// A legitimate entry to confirm the unpack still works.
	writeZipFile(t, zw, winDistro+"/node.exe", []byte("ok"), 0o755)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	dest := t.TempDir()
	if err := unpackArchive(buf.Bytes(), "zip", winDistro, dest); err != nil {
		t.Fatalf("unpackArchive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "node.exe")); err != nil {
		t.Fatalf("legitimate entry not unpacked: %v", err)
	}
	parent := filepath.Dir(dest)
	for _, name := range []string{"escape.txt", "escape2.txt"} {
		if _, err := os.Stat(filepath.Join(parent, name)); err == nil {
			t.Fatalf("traversal entry %q escaped the unpack target", name)
		}
	}
}

func TestUnpackTarGz_RecreatesInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	data := tarGz(t, func(tw *tar.Writer) {
		writeTarDir(t, tw, testDistro+"/")
		writeTarDir(t, tw, testDistro+"/bin/")
		writeTarFile(t, tw, testDistro+"/bin/node", []byte("#!node"), 0o755)
		// A relative symlink that stays inside the tree, as real Node
		// archives contain (e.g. npx -> ../lib/node_modules/...).
		if err := tw.WriteHeader(&tar.Header{
			Name:     testDistro + "/bin/nodealias",
			Typeflag: tar.TypeSymlink,
			Linkname: "node",
			Mode:     0o777,
		}); err != nil {
			t.Fatalf("write symlink header: %v", err)
		}
	})

	dest := t.TempDir()
	if err := unpackArchive(data, "tar.gz", testDistro, dest); err != nil {
		t.Fatalf("unpackArchive: %v", err)
	}
	link := filepath.Join(dest, "bin", "nodealias")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "node" {
		t.Fatalf("symlink target = %q, want %q", got, "node")
	}
	// And it resolves to the real file.
	body, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read through symlink: %v", err)
	}
	if string(body) != "#!node" {
		t.Fatalf("resolved symlink content = %q", body)
	}
}

func TestUnpackArchive_UnsupportedExtension(t *testing.T) {
	err := unpackArchive([]byte("x"), "rar", testDistro, t.TempDir())
	if err == nil {
		t.Fatalf("unpackArchive accepted an unsupported extension")
	}
	var ndErr *Error
	if !errors.As(err, &ndErr) || ndErr.Kind != ErrUnpack {
		t.Fatalf("error = %v, want ErrUnpack", err)
	}
}

func TestNodeBinaryPath_WindowsAndUnix(t *testing.T) {
	unix := nodeBinaryPath("/cache/abc", Target{GOOS: "linux", GOARCH: "amd64"})
	if unix != filepath.Join("/cache/abc", "bin", "node") {
		t.Errorf("unix node binary path = %q", unix)
	}
	win := nodeBinaryPath("/cache/abc", Target{GOOS: "windows", GOARCH: "amd64"})
	if win != filepath.Join("/cache/abc", "node.exe") {
		t.Errorf("windows node binary path = %q", win)
	}
}

func TestWriteRegular_EnforcesRemainingCap(t *testing.T) {
	dir := t.TempDir()

	// remaining <= 0 is rejected immediately.
	if _, err := writeRegular(filepath.Join(dir, "a"), bytes.NewReader([]byte("x")), 0o644, 0); err == nil {
		t.Fatalf("writeRegular accepted remaining=0")
	}

	// A body larger than remaining is rejected as ErrUnpack.
	body := bytes.Repeat([]byte("z"), 100)
	_, err := writeRegular(filepath.Join(dir, "b"), bytes.NewReader(body), 0o644, 10)
	if err == nil {
		t.Fatalf("writeRegular accepted a body exceeding the cap")
	}
	var ndErr *Error
	if !errors.As(err, &ndErr) || ndErr.Kind != ErrUnpack {
		t.Fatalf("error = %v, want ErrUnpack", err)
	}

	// A body within the cap is written verbatim.
	n, err := writeRegular(filepath.Join(dir, "c"), bytes.NewReader([]byte("hello")), 0o644, 100)
	if err != nil {
		t.Fatalf("writeRegular within cap: %v", err)
	}
	if n != 5 {
		t.Fatalf("writeRegular wrote %d bytes, want 5", n)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "c"))
	if string(got) != "hello" {
		t.Fatalf("written content = %q", got)
	}
}

func writeTarDir(t *testing.T, tw *tar.Writer, name string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatalf("write dir %s: %v", name, err)
	}
}

func writeTarFile(t *testing.T, tw *tar.Writer, name string, body []byte, mode int64) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     mode,
		Size:     int64(len(body)),
	}); err != nil {
		t.Fatalf("write header %s: %v", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write body %s: %v", name, err)
	}
}

func writeZipFile(t *testing.T, zw *zip.Writer, name string, body []byte, mode int64) {
	t.Helper()
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(os.FileMode(mode).Perm())
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatalf("create zip entry %s: %v", name, err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("write zip entry %s: %v", name, err)
	}
}
