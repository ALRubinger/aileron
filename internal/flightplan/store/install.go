package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/flightplan/resolver"
)

// InstallResult describes the outcome of an Install.
type InstallResult struct {
	// Name is the installed skill's name.
	Name string
	// Dir is the on-disk directory the skill was written to.
	Dir string
	// InstructionOnly reports whether the skill carried no aileron block.
	// Instruction-only skills skip requires-resolution entirely.
	InstructionOnly bool
	// Unsatisfied lists the action refs that did not resolve against the
	// running daemon. Non-empty means the install degraded: the skill is
	// still installed, but its requires are not all satisfiable here.
	Unsatisfied []string
	// ResolverConsulted reports whether the requires-resolver (and thus the
	// daemon) was consulted. It is false for instruction-only skills.
	ResolverConsulted bool
}

// Degraded reports whether the install resolved with unsatisfied refs.
func (r InstallResult) Degraded() bool { return len(r.Unsatisfied) > 0 }

// InstallOptions configures an Install.
type InstallOptions struct {
	// Fetcher resolves the skill's action requirements against the running
	// daemon. It is consulted ONLY for skills that declare an aileron block
	// with requires. Instruction-only skills never reach it (P2 isolation:
	// a daemon-down instruction-only install must be clean and silent).
	// A nil Fetcher means "do not resolve": refs are recorded as
	// unsatisfied without a daemon call (used when no daemon is reachable
	// and the caller wants install-anyway behavior).
	Fetcher resolver.Fetcher

	// git overrides the git runner (seam for tests). Nil uses runGit.
	git gitRunner
	// slug overrides the slug resolver (seam for tests). Nil uses the v0
	// not-wired resolver.
	slug slugResolver
}

// Install fetches a skill from src (local path, git URL, or — once wired —
// an agentskills.io slug), parses and validates it, resolves its action
// requirements when it declares any, and writes it into the canonical
// store under its skill name.
//
// Degrade, not block: an unsatisfiable requires warns (recorded in
// InstallResult.Unsatisfied) but still installs. An instruction-only skill
// (no aileron block) skips the resolver and any daemon call entirely.
//
// Re-installing a skill of the same name overwrites it (idempotent), which
// keeps the #1509 freeze flow simple.
func (s *Store) Install(ctx context.Context, src string, opts InstallOptions) (InstallResult, error) {
	git := opts.git
	if git == nil {
		git = runGit
	}
	slug := opts.slug
	if slug == nil {
		slug = notWiredSlugResolver
	}

	workDir, err := os.MkdirTemp("", "aileron-skill-install-")
	if err != nil {
		return InstallResult{}, fmt.Errorf("store: temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	srcDir, err := fetchToDir(ctx, src, workDir, git, slug)
	if err != nil {
		return InstallResult{}, err
	}

	raw, err := os.ReadFile(filepath.Join(srcDir, "SKILL.md"))
	if err != nil {
		return InstallResult{}, fmt.Errorf("store: read SKILL.md: %w", err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return InstallResult{}, fmt.Errorf("store: parse skill: %w", err)
	}
	if m.Name == "" {
		return InstallResult{}, fmt.Errorf("store: skill has no name in its frontmatter")
	}

	result := InstallResult{
		Name:            m.Name,
		Dir:             s.Dir(m.Name),
		InstructionOnly: m.InstructionOnly,
	}

	// Requires-resolution. Instruction-only skills (and skills that declare
	// no action refs) short-circuit BEFORE any daemon call so a daemon-down
	// instruction-only install is clean and silent.
	refs := m.Refs()
	if len(refs) > 0 {
		if opts.Fetcher != nil {
			res, err := resolver.ResolveWith(ctx, opts.Fetcher, refs)
			if err != nil {
				return InstallResult{}, fmt.Errorf("store: resolve requires: %w", err)
			}
			result.ResolverConsulted = true
			result.Unsatisfied = res.Unsatisfied
		} else {
			// No fetcher: record all refs as unsatisfied without a daemon
			// call. The caller warns and installs anyway (degrade, not block).
			result.Unsatisfied = append(result.Unsatisfied, refs...)
		}
	}

	// Write into the canonical store. Overwrite any prior install of the
	// same name (idempotent re-install).
	dst := s.Dir(m.Name)
	if err := os.RemoveAll(dst); err != nil {
		return InstallResult{}, fmt.Errorf("store: clear prior install: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("store: create skill dir: %w", err)
	}
	if err := copyTree(srcDir, dst); err != nil {
		return InstallResult{}, fmt.Errorf("store: write skill content: %w", err)
	}

	return result, nil
}
