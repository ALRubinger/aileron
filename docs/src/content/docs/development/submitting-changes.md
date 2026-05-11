---
title: "Submitting Changes"
description: "Branch and PR conventions, commit message format, ADR amendments, and what to expect from review."
order: 5
---

Aileron is open source and accepts contributions. This page is the contributor's checklist: what shape a change should take before you open the PR, and what reviewers look for.

## Branch from main

Every change starts as a branch off `main`. The repo uses `worktree-<name>` branches by convention for in-flight work, but any clear branch name is fine.

```sh
git fetch origin main
git checkout -b my-change origin/main
```

Force-pushing the branch during review is fine and encouraged for clean history. Force-pushing `main` is forbidden — it breaks downstream consumers.

## Conventional Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org). The format is:

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

The recognized types are:

| Type | When |
|---|---|
| `feat` | A new feature. |
| `fix` | A bug fix. |
| `docs` | Documentation-only changes. |
| `style` | Formatting or whitespace. No logic change. |
| `refactor` | Code change that is neither a fix nor a feature. |
| `perf` | Performance improvement. |
| `test` | Adding or correcting tests. |
| `build` | Build system or dependency changes. |
| `ci` | CI configuration changes. |
| `chore` | Other changes that don't modify src or test files. |
| `revert` | Reverts a previous commit. |

Rules:

- **description** is lowercase, imperative, no trailing period, ≤72 characters.
- **scope** is optional. When used, it's a lowercase noun in parentheses: `feat(sandbox): ...`.
- **breaking change** appends `!` after the type or scope: `feat!: drop Node 16 support`. The footer `BREAKING CHANGE: <description>` is required.
- **body and footers** are separated from the description by a blank line.

The repo's recent commits are the best reference.

## Test coverage before opening the PR

The working bar is:

- **All tests pass.** `task test:go:ci` reproduces what CI runs.
- **New code has a happy-path test.** A function that ships without an end-to-end exercise of its primary use case is incomplete.
- **Bug fixes have a regression test.** It must fail before the fix and pass after.
- **>80% coverage on new code** as the default, with the explicit exception that filesystem-error wrappers and similarly brittle paths are not metric-chased. See [Running Tests](/development/running-tests/) for the philosophy.

## ADR amendments

The architecture decision records under [Architecture Decisions](/adr/) are pre-MVP and editable in place. If your change touches a design commitment in one of the ADRs, amend it in the same PR. Don't supersede an ADR with a new one until the MVP ships.

Conventions:

- A new ADR is a separate PR scoped to the decision (research, alternatives, consequences).
- Every `ADR-NNNN` reference in any ADR is a Markdown link to that ADR's page.
- An ADR's `Status` stays `Accepted` pre-MVP even when amended. Once MVP ships, supersession becomes the rule.

## Pull request

```sh
gh pr create --title "<conventional-commit-style title>" --body "..."
```

The PR body should answer:

- **What changed and why.** A summary, not a diff readout.
- **How it was tested.** What you ran locally. What CI will run.
- **Open questions.** Anything you want the reviewer to weigh in on specifically.

CI runs on every push. The required gates are Lint & Verify, Unit Tests (Linux), Unit Tests (Windows), Integration Tests, UI Tests, and Docs. The Socket Security checks and codecov reports run advisory; codecov in particular is sometimes a metric-chase signal rather than a quality signal, and a low patch-coverage number on filesystem-error paths is not a blocker.

## Review

Aileron is a small project. Review is direct and aims for fast turnaround. Reviewers look for:

- **Does this match the contract?** Tests, docs, and the ADRs are the contract; the code follows them.
- **Is the change scoped?** Bug fixes don't include opportunistic refactors. Features don't include unrelated formatting passes.
- **Is anything load-bearing missing?** A regression test for a bug fix. An ADR amendment when a design commitment changes. A docs update when a user-visible behavior changes.

If a reviewer requests changes, push fixes as new commits on the branch (not amends). Squash-merge folds them into a single conventional commit at merge time.

## When you're not sure

Open the PR anyway with a clear "I'm not sure about X" in the body. Reviewing in flight beats accumulating uncertainty offline.

## See also

- [Running Tests](/development/running-tests/)
- [Building from Source](/development/building-from-source/)
- [Architecture Decisions](/adr/)
