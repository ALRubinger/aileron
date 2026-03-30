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

# UI Conventions

The management UI (`ui/`) uses **SvelteKit** with **Svelte 5 runes**.

## Svelte 5 Runes — Required Patterns

Always use Svelte 5 rune syntax. Never use Svelte 4 reactive declarations.

| Concept | Svelte 5 (use this) | Svelte 4 (never use) |
|---------|--------------------|--------------------|
| Local state | `let count = $state(0)` | `let count = 0` |
| Derived value | `let double = $derived(count * 2)` | `$: double = count * 2` |
| Side effect | `$effect(() => { ... })` | `$: { ... }` |
| Component props | `let { name, age } = $props()` | `export let name` |
| Typed props | `let { name }: { name: string } = $props()` | `export let name: string` |

## Component conventions

- All components use `<script lang="ts">`
- Props are always destructured from `$props()`
- Child content uses `Snippet` type: `let { children }: { children: Snippet } = $props()`
- Render snippets with `{@render children()}`
- Route files follow SvelteKit conventions: `+page.svelte`, `+page.server.ts`, `+layout.svelte`, `+server.ts`

---

# Commit Message Format

## Examples

```
feat(api): add pagination to list endpoint

fix: handle null pointer in user lookup

docs: update contributing guide

chore!: drop support for Node 16

BREAKING CHANGE: Node 16 is no longer supported.
```
