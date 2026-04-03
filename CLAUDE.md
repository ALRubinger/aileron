# OpenAPI Spec is the Source of Truth

The OpenAPI specification at `core/api/openapi.yaml` is the authoritative definition of the Aileron API. All API changes — new endpoints, schema modifications, parameter changes — **must** be made in the spec first. The Go server interface and types are generated from it:

```sh
task generate:api
```

Never hand-edit `core/api/gen/server.gen.go`. If the spec and the code diverge, the spec wins. Regenerate after every spec change to keep them in sync.

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
