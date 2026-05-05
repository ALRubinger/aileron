# Aileron local webapp

The webapp surfaced under `aileron launch` for action-approval review
and decision. Built statically (`@sveltejs/adapter-static`); embedded
into the daemon binary; served at `/` by the same gateway the agent
talks to. No external dev server required at runtime.

## Dev loop

While iterating on the webapp itself, run the SvelteKit dev server
alongside the daemon — hot-reload + source maps without rebuilding the
Go binary on every change:

```sh
# Terminal 1: launch the daemon as usual.
./build/aileron launch claude

# Terminal 2: webapp dev server.
task dev:webapp
# webapp at http://localhost:5173 — talks to the daemon's API on
# its dynamic port via the standard fetch (same-origin in production,
# CORS during dev).
```

When you're done iterating, refresh the embedded build so the daemon
serves the new assets:

```sh
task build:webapp   # builds + copies into internal/app/webapp_dist/
task build:server   # rebuilds daemon with the new embed
```

`task build:server` declares `build:webapp` as a dependency, so any
top-level `task build` always picks up webapp changes.

## What's in scope here

- `/approvals` — the pending action-approval queue. SSE-driven; the
  agent's blocked tool calls show up live without page refresh.
- `/` — landing page that points at `/approvals`.

That's it for v0.x. The cloud-tier surfaces (auth, organization
settings, billing, multi-user vault) live in `../ui/` (frozen) and
will be rebuilt from first principles when cloud-hosted Aileron is
its own concern.

## Stack

- SvelteKit 2 + Svelte 5 (runes mode)
- `@sveltejs/adapter-static` (no Node server, no `+page.server.ts`,
  no form actions — every page must be statically prerenderable)
- Tailwind 4
- bits-ui (Shadcn-style component primitives, copied from `../ui/`)
- vitest + @testing-library/svelte for unit tests

## Files of note

- `src/lib/api.ts` — same-origin fetch + SSE subscriber for
  `/v1/action-approvals/watch`. No JWT, no token refresh, no
  multi-user concerns; the daemon-vault model means the local
  surface is single-user and the vault is unlocked at launch.
- `src/routes/approvals/+page.svelte` — the load-bearing page.
  Subscribes to the SSE stream, renders pending approvals as cards
  with inline approve/deny + optional reason input.
- `src/lib/components/ui/` — Shadcn-style primitives (button, card,
  input, etc.). Copied verbatim from `../ui/src/lib/components/ui/`.

## Why a separate webapp/, not in ui/?

`ui/` was built for cloud-hosted Aileron — multi-user auth, OAuth
callbacks, organization settings. The local webapp is single-user,
trusted-by-default (the daemon serves it at the same origin the
vault is unlocked on), and small enough that pruning `ui/` would
have thrown away ~40% of routes irreversibly. Forking gives the
local webapp a clean slate; `ui/` stays in tree as a frozen reference
for the eventual cloud rebuild.

The two share the SvelteKit + Tailwind + bits-ui foundation — every
component file under `lib/components/ui/` is byte-identical between
`ui/` and `webapp/` so the cosmetic vocabulary stays consistent.
