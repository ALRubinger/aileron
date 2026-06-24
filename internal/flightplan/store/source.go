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
// attempted. The slug source is a seam in v0: there is no pinned
// agentskills.io wire contract in the repo yet, so the concrete fetch is
// deferred to a named follow-up rather than invented here. See issue #1583.
var ErrSlugNotWired = errors.New(
	"store: installing by agentskills.io slug is not wired yet; " +
		"use a local path or a git URL for now (tracked by #1583)")

// SourceKind classifies an install source string.
type SourceKind int

const (
	// SourceLocalPath is a filesystem path to a skill directory (a
	// directory containing SKILL.md) or directly to a SKILL.md file.
	SourceLocalPath SourceKind = iota
	// SourceGitURL is a remote git repository URL to clone.
	SourceGitURL
	// SourceSlug is an agentskills.io registry slug (owner/name).
	SourceSlug
)

// classifySource decides how to interpret an install source string.
//
//   - A string with a git URL shape (scheme://… or git@host:…, or a .git
//     suffix) is a git URL.
//   - A string that resolves to an existing filesystem path is a local path.
//   - Anything else with an `owner/name` shape is treated as a registry slug.
func classifySource(src string) SourceKind {
	if isGitURL(src) {
		return SourceGitURL
	}
	if _, err := os.Stat(src); err == nil {
		return SourceLocalPath
	}
	return SourceSlug
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
// seam: v0 returns ErrSlugNotWired. See issue #1583 for the wire contract.
type slugResolver func(ctx context.Context, slug, destDir string) (string, error)

// notWiredSlugResolver is the v0 slug resolver.
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
