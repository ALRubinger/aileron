package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrSlugNotWired is returned when an agentskills.io slug install is
// attempted. Slug install is intentionally unsupported: agentskills.io is a
// format spec, not a registry, so a slug resolves to no canonical location.
// The only implementable shape would be GitHub-as-registry, which is pure
// sugar over the git-URL source that already covers distribution. Decided
// won't-do in #1583; revisit only if a real agentskills.io registry ships.
var ErrSlugNotWired = errors.New(
	"store: installing by agentskills.io slug is not supported; " +
		"use a local path or a git URL")

// SourceKind classifies an install source string.
type SourceKind int

const (
	// SourceLocalPath is a filesystem path to a skill directory (a
	// directory containing SKILL.md) or directly to a SKILL.md file.
	SourceLocalPath SourceKind = iota
	// SourceGitURL is a remote git repository URL to clone.
	SourceGitURL
	// SourceOCIRef is an OCI reference to a published Flight Plan artifact
	// (`<registry-host>/<repo>:<version>`), installed by pull + verify + write.
	SourceOCIRef
	// SourceSlug is an agentskills.io registry slug (owner/name).
	SourceSlug
)

// classifySource decides how to interpret an install source string.
//
//   - A string with a git URL shape (scheme://… or git@host:…, or a .git
//     suffix) is a git URL.
//   - A string that resolves to an existing filesystem path is a local path.
//   - A string shaped like an OCI reference (a registry host with a dot,
//     `:port`, or `localhost`, plus a `:version` tag) is an OCI reference to a
//     published Flight Plan.
//   - Anything else with an `owner/name` shape is treated as a registry slug.
//
// Order matters: a bare `owner/name` slug carries no registry host and no tag,
// so it still classifies as SourceSlug (→ ErrSlugNotWired, the won't-do #1583
// boundary), while `ghcr.io/owner/name:v1` classifies as an OCI reference.
func classifySource(src string) SourceKind {
	if isGitURL(src) {
		return SourceGitURL
	}
	if _, err := os.Stat(src); err == nil {
		return SourceLocalPath
	}
	if isOCIRef(src) {
		return SourceOCIRef
	}
	return SourceSlug
}

// IsOCIReference reports whether src is shaped like an OCI reference to a
// published Flight Plan artifact (see isOCIRef). The CLI uses it to route an
// install to the pull path (verify + WriteFrozen) instead of the local/git
// copyTree install. It is a string-only heuristic; the pull package does the
// authoritative registry.ParseReference.
func IsOCIReference(src string) bool {
	// Git URLs and existing local paths take precedence, matching
	// classifySource's ordering, so a `.git` URL or a real file/dir is never
	// mistaken for an OCI ref.
	if isGitURL(src) {
		return false
	}
	if _, err := os.Stat(src); err == nil {
		return false
	}
	return isOCIRef(src)
}

// isOCIRef reports whether src is shaped like an OCI reference to a published
// Flight Plan: a `<registry-host>/<repo>:<tag>` where the registry host looks
// like a network host (contains a dot, carries a `:port`, or is `localhost`)
// and a non-empty tag follows the repository path.
//
// This is a string-only heuristic so the store stays free of the oras
// dependency; the pull package does the authoritative registry.ParseReference.
// The host-shape requirement is the disambiguator against a bare `owner/name`
// slug: a slug has no host segment and no tag, so it never matches here.
func isOCIRef(src string) bool {
	slash := strings.IndexByte(src, '/')
	if slash <= 0 {
		// No repository path: not a `<host>/<repo>` shape.
		return false
	}
	host := src[:slash]
	rest := src[slash+1:]
	if rest == "" {
		return false
	}
	// A digest reference (@sha256:…) has no version tag the store can key on, so
	// it is not recognized here (pull requires a tag).
	if strings.ContainsRune(rest, '@') {
		return false
	}
	// The tag is the final `:tag` segment of the repository path.
	tagSep := strings.LastIndexByte(rest, ':')
	if tagSep < 0 || tagSep == len(rest)-1 {
		return false
	}
	if strings.ContainsAny(rest[tagSep+1:], "/@") {
		// A colon that precedes a further path or digest is not a tag.
		return false
	}
	return isRegistryHost(host)
}

// isRegistryHost reports whether host looks like an OCI registry host: it is
// `localhost` (optionally with a port), or contains a dot (a domain) or a
// `:port`. A bare single label like `owner` is not a registry host, so a slug's
// first segment never matches.
func isRegistryHost(host string) bool {
	if host == "" {
		return false
	}
	bare := host
	if i := strings.LastIndexByte(bare, ':'); i >= 0 {
		if i == 0 {
			return false
		}
		bare = bare[:i]
	}
	if bare == "localhost" {
		return true
	}
	// A dot (domain) or a port on the original host qualifies.
	return strings.ContainsRune(bare, '.') || strings.ContainsRune(host, ':')
}

// isGitURL reports whether src looks like a git remote URL rather than a
// local path or a bare slug.
func isGitURL(src string) bool {
	switch {
	case strings.HasSuffix(src, ".git"):
		return true
	case strings.HasPrefix(src, "git@"):
		return true
	case strings.Contains(src, "://"):
		// Any scheme://… (https://, ssh://, git://, file://) is a URL.
		return true
	default:
		return false
	}
}

// gitRunner runs a git command. It is a seam so tests can drive the
// git-URL path without a network clone; production uses runGit.
type gitRunner func(ctx context.Context, args ...string) error

// runGit shells out to the git binary.
func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// slugResolver resolves an agentskills.io slug to fetched skill content on
// disk, returning the local directory the content was fetched into. It is a
// seam, but slug install is unsupported by decision (#1583, won't-do): the
// resolver returns ErrSlugNotWired.
type slugResolver func(ctx context.Context, slug, destDir string) (string, error)

// notWiredSlugResolver is the slug resolver. Slug install is unsupported by
// decision (#1583); it always returns ErrSlugNotWired.
func notWiredSlugResolver(_ context.Context, _, _ string) (string, error) {
	return "", ErrSlugNotWired
}

// fetchToDir materializes the install source into a directory containing a
// SKILL.md and returns that directory. For a local path it returns the
// source directory directly (no copy until the store write). For a git URL
// it clones into workDir. For a slug it calls the slug resolver.
func fetchToDir(ctx context.Context, src string, workDir string, git gitRunner, slug slugResolver) (string, error) {
	switch classifySource(src) {
	case SourceLocalPath:
		return localSourceDir(src)
	case SourceGitURL:
		clone := filepath.Join(workDir, "clone")
		if err := git(ctx, "clone", "--depth", "1", "--single-branch", src, clone); err != nil {
			return "", err
		}
		return skillDirWithin(clone)
	case SourceOCIRef:
		// An OCI reference installs a published FROZEN version through the pull
		// path (verify + WriteFrozen), not the pre-freeze copyTree install this
		// function drives. The CLI branches to pull before calling Install, so
		// Install must never receive an OCI ref; surface a clear error if it does.
		return "", fmt.Errorf("store: %q is an OCI reference; install it through the frozen-version pull path, not Install", src)
	case SourceSlug:
		return slug(ctx, src, workDir)
	default:
		return "", fmt.Errorf("store: unrecognized source %q", src)
	}
}

// localSourceDir resolves a local install source to the directory that
// contains its SKILL.md. The source may point at the directory or directly
// at the SKILL.md file.
func localSourceDir(src string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("store: stat source %q: %w", src, err)
	}
	if info.IsDir() {
		return skillDirWithin(src)
	}
	// A file path: it must be a SKILL.md.
	if filepath.Base(src) != "SKILL.md" {
		return "", fmt.Errorf("store: source file %q is not a SKILL.md", src)
	}
	return filepath.Dir(src), nil
}

// skillDirWithin verifies dir contains a SKILL.md and returns dir.
func skillDirWithin(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return "", fmt.Errorf("store: no SKILL.md in %q", dir)
	}
	return dir, nil
}

// copyTree recursively copies the regular files and subdirectories under
// src into dst. Symlinks are skipped (the store holds plain skill content;
// the codebase avoids symlinks in repos and the store mirrors that). It is
// the store's write primitive.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		// Skip a .git directory entirely. A git-URL install clones into a
		// working tree whose root carries .git; the store holds skill
		// content only, not version-control metadata, so copying it in
		// would bloat the store with the whole history.
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			// Skip non-regular files (symlinks, devices, sockets).
			return nil
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
