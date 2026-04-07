# OpenAPI Spec is the Source of Truth

The OpenAPI specification at `core/api/openapi.yaml` is the authoritative definition of the Aileron API. All API changes — new endpoints, schema modifications, parameter changes — **must** be made in the spec first. The Go server interface and types are generated from it:

```sh
task generate:api
```

Never hand-edit `core/api/gen/server.gen.go`. If the spec and the code diverge, the spec wins. Regenerate after every spec change to keep them in sync.

# Testing Philosophy

Tests must be written against the **contract** of the code — the expected inputs, outputs, side effects, and error conditions defined by the function signature, API spec, or documentation. Never write tests against implementation internals.

## Rules

- **Define expected behavior first.** Before writing a test, state what the function should do: given these inputs, what outputs and side effects are expected? What error conditions does the contract define?
- **Test the happy path completely.** Verify the feature produces correct results under normal conditions. If a test can't exercise the happy path due to a dependency (e.g. a hardcoded external URL), fix the dependency — add test hooks, mock endpoints, inject configuration — rather than asserting on the failure.
- **Test exceptional conditions from the contract.** Every error code, validation rule, or edge case documented in the API spec or function signature should have a test. These are the contract's stated failure modes.
- **Never assert on implementation accidents.** If a test passes because of how the code happens to be structured (e.g. "this fails because it tries to reach Google"), that's not a test — it's a mirror. It tells you nothing about whether the feature works and breaks when the implementation changes.
- **Tests must survive refactoring.** If the implementation changes but the contract stays the same, all tests should still pass. If they don't, the tests were coupled to internals, not behavior.

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
