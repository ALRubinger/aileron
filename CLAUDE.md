# Commit Message Format

All commits must use **Conventional Commits** format:

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

## Types

| Type | When to use |
|------|-------------|
| `feat` | New feature |
| `fix` | Bug fix |
| `docs` | Documentation changes only |
| `style` | Formatting, missing semicolons, etc. (no logic change) |
| `refactor` | Code change that is neither a fix nor a feature |
| `perf` | Performance improvement |
| `test` | Adding or correcting tests |
| `build` | Build system or dependency changes |
| `ci` | CI configuration changes |
| `chore` | Other changes that don't modify src or test files |
| `revert` | Reverts a previous commit |

## Rules

- **description**: lowercase, imperative mood, no trailing period, ≤72 chars on first line
- **scope**: optional, lowercase noun in parentheses — e.g. `feat(auth): add login`
- **breaking change**: append `!` after type/scope — e.g. `feat!: drop Node 16 support`
- **body/footers**: separated from description by a blank line
- `BREAKING CHANGE: <description>` footer required for breaking changes

## Examples

```
feat(api): add pagination to list endpoint

fix: handle null pointer in user lookup

docs: update contributing guide

chore!: drop support for Node 16

BREAKING CHANGE: Node 16 is no longer supported.
```
