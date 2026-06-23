package nodedist

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxUnpackBytes caps the total decompressed size of a Node archive to guard
// against a decompression bomb. A full Node distribution is well under this.
const maxUnpackBytes = 1 << 30 // 1 GiB

// unpackArchive extracts a Node archive (raw bytes) into dest, stripping the
// top-level distroPrefix directory (e.g. "node-v24.2.0-linux-x64") so dest
// ends up with bin/, lib/, etc. at its root. ext selects the format:
// "tar.gz" or "zip".
//
// dest must already exist and be empty. Every entry is checked against
// zip-slip path traversal: an entry that resolves outside dest aborts the
// unpack with an ErrUnpack error before any bytes for it are written.
// Executable bits are preserved from the archive's recorded mode.
func unpackArchive(data []byte, ext, distroPrefix, dest string) error {
	switch ext {
	case "tar.gz":
		return unpackTarGz(data, distroPrefix, dest)
	case "zip":
		return unpackZip(data, distroPrefix, dest)
	default:
		return wrapErr(ErrUnpack, nil, "unsupported archive extension %q", ext)
	}
}

func unpackTarGz(data []byte, distroPrefix, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return wrapErr(ErrUnpack, err, "open gzip stream")
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var written int64
	for {
		hdr, hErr := tr.Next()
		if errors.Is(hErr, io.EOF) {
			break
		}
		if hErr != nil {
			return wrapErr(ErrUnpack, hErr, "read tar entry")
		}
		rel, ok := stripPrefix(hdr.Name, distroPrefix)
		if !ok || rel == "" {
			continue
		}
		target, sErr := safeJoin(dest, rel)
		if sErr != nil {
			return sErr
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return wrapErr(ErrUnpack, err, "mkdir %s", rel)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return wrapErr(ErrUnpack, err, "mkdir parent of %s", rel)
			}
			n, wErr := writeRegular(target, tr, fileMode(hdr.Mode), maxUnpackBytes-written)
			written += n
			if wErr != nil {
				return wErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Node Unix archives contain symlinks (e.g. lib/node_modules
			// internals). Recreate them, but only when the target stays
			// inside dest.
			if err := writeSymlink(dest, target, hdr.Linkname); err != nil {
				return err
			}
		default:
			// Skip devices, fifos, etc. — not present in Node archives.
			continue
		}
	}
	return nil
}

func unpackZip(data []byte, distroPrefix, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return wrapErr(ErrUnpack, err, "open zip archive")
	}
	var written int64
	for _, f := range zr.File {
		rel, ok := stripPrefix(f.Name, distroPrefix)
		if !ok || rel == "" {
			continue
		}
		target, sErr := safeJoin(dest, rel)
		if sErr != nil {
			return sErr
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return wrapErr(ErrUnpack, err, "mkdir %s", rel)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return wrapErr(ErrUnpack, err, "mkdir parent of %s", rel)
		}
		rc, oErr := f.Open()
		if oErr != nil {
			return wrapErr(ErrUnpack, oErr, "open zip entry %s", rel)
		}
		n, wErr := writeRegular(target, rc, f.Mode(), maxUnpackBytes-written)
		rc.Close()
		written += n
		if wErr != nil {
			return wErr
		}
	}
	return nil
}

// stripPrefix removes the leading "<distroPrefix>/" segment from a slash-
// separated archive entry name. Returns (rel, true) when name was under the
// prefix; (_, false) otherwise (such entries are skipped). The prefix itself
// (the bare top-level dir) yields ("", true).
func stripPrefix(name, distroPrefix string) (string, bool) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == distroPrefix {
		return "", true
	}
	prefix := distroPrefix + "/"
	if !strings.HasPrefix(clean, prefix) {
		return "", false
	}
	return strings.TrimPrefix(clean, prefix), true
}

// safeJoin joins dest and a relative path, rejecting any result that escapes
// dest (zip-slip / "../" path traversal).
func safeJoin(dest, rel string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(rel))
	// filepath.Join cleans the path; verify it is still under dest.
	cleanDest := filepath.Clean(dest)
	if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
		return "", wrapErr(ErrUnpack, nil, "archive entry %q escapes the unpack target", rel)
	}
	return target, nil
}

// writeRegular streams src into a new file at target with mode, capping the
// number of bytes at remaining to bound decompression. Returns the number of
// bytes written.
func writeRegular(target string, src io.Reader, mode os.FileMode, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, wrapErr(ErrUnpack, nil, "archive exceeds maximum unpacked size")
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return 0, wrapErr(ErrUnpack, err, "create %s", target)
	}
	// +1 so we can detect overflow past the cap.
	n, cErr := io.Copy(f, io.LimitReader(src, remaining+1))
	if closeErr := f.Close(); closeErr != nil && cErr == nil {
		cErr = closeErr
	}
	if cErr != nil {
		return n, wrapErr(ErrUnpack, cErr, "write %s", target)
	}
	if n > remaining {
		return n, wrapErr(ErrUnpack, nil, "archive exceeds maximum unpacked size")
	}
	return n, nil
}

// writeSymlink recreates a symlink at target pointing to linkname, but only
// when the resolved destination stays within root. A symlink escaping root
// is rejected as a path-traversal attempt.
func writeSymlink(root, target, linkname string) error {
	resolved := linkname
	if !filepath.IsAbs(linkname) {
		resolved = filepath.Join(filepath.Dir(target), linkname)
	}
	cleanRoot := filepath.Clean(root)
	if resolved != cleanRoot && !strings.HasPrefix(filepath.Clean(resolved), cleanRoot+string(os.PathSeparator)) {
		return wrapErr(ErrUnpack, nil, "symlink %q -> %q escapes the unpack target", target, linkname)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return wrapErr(ErrUnpack, err, "mkdir parent for symlink")
	}
	// Replace any existing entry so repeated unpacks into a fresh dir are
	// deterministic (the dir is always fresh in practice).
	_ = os.Remove(target)
	if err := os.Symlink(linkname, target); err != nil {
		return wrapErr(ErrUnpack, err, "create symlink %s", target)
	}
	return nil
}

// fileMode converts a tar header mode (an int64 of Unix permission bits)
// into an os.FileMode, preserving the executable bits.
func fileMode(m int64) os.FileMode {
	return os.FileMode(m).Perm()
}
