# `ui/` — frozen cloud-tier reference UI

This directory is **not actively maintained** and is **not built into
the Aileron daemon**. The local webapp Aileron serves under
`aileron launch` lives in `../webapp/`; that's where new work belongs.

## Why `ui/` is here

`ui/` was built for cloud-hosted Aileron — multi-user auth, OAuth
callbacks, organization settings, billing-tier connected accounts,
TEE attestation transparency, traces. Per the strategic pivot in
[issue #335](https://github.com/ALRubinger/aileron/issues/335), the
local-first experience is the v0.x focus; the cloud-hosted variant
will be rebuilt from first principles when its time comes.

Rather than delete `ui/` and lose the working SvelteKit + Tailwind +
bits-ui scaffolding that the cloud rebuild can reference, it's
preserved here. Routes worth referencing for the eventual rebuild:

- `src/routes/login`, `signup`, `verify-email`, `auth/callback` — the
  multi-user auth lifecycle.
- `src/routes/settings/organization` — enterprise billing, SSO, org-
  level LLM config.
- `src/routes/settings/connected-accounts` — OAuth token management
  for the cloud vault model.
- `src/routes/settings/security` — TEE attestation transparency
  (image digest, project id, hwmodel).
- `src/lib/crypto/` — client-side vault crypto (argon2, ECDH,
  attestation verification).
- `src/lib/auth.svelte.js` — token-refresh + cookie-auth handling.

## What you should do here

- **For local-webapp work**: don't. Use `../webapp/` instead.
- **For dependency upgrades**: still useful to occasionally bump
  `ui/` so the components stay compatible with the rest of the
  Svelte ecosystem. Low priority.
- **For the cloud rebuild**: cherry-pick patterns from here as
  reference; don't expect `ui/` to be a working starting point —
  the strategic intent is "rebuild from first principles."

## Tasks

The `task build:ui`, `task lint:ui`, `task dev:ui`, and `task test:ui`
targets are kept so the existing CI workflow (which type-checks `ui/`)
keeps passing without modification. `ui/` is not part of `task build`'s
default cmd list, and is not a dependency of `task build:server`.

## License

Same as the rest of the repo — see [`../LICENSE`](../LICENSE).
