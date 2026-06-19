# Published sandbox-base compose layer (umbrella #1319)

The published `aileron-sandbox-base` image is built in two stages:

1. **Substrate** — `images/sandbox-base/Containerfile.published` builds the
   repo-root, multi-arch, mcp-baked substrate exactly as before, minus
   `github-cli` (gh now ships as a Feature, not an apk line).
2. **Compose** — a thin `@devcontainers/cli build` step does `FROM <substrate>`
   and composes the `images/sandbox-features/gh` Feature on top. That step
   installs `gh` and emits the `devcontainer.metadata` image label carrying gh's
   CLI-capability unit (`customizations.aileron.cli`), which the unit loader
   (#1322) reads at launch/auth time.

`sandbox-base.yml` renders `devcontainer.json` from `devcontainer.json.tmpl`
(substituting the per-arch substrate ref) into a scratch workspace alongside a
copy of the gh Feature, then runs `devcontainer build --platform linux/<arch>`
per architecture and assembles the published multi-arch manifest with
`docker buildx imagetools create`.

The empirical basis (probed before implementing, umbrella #1319):
- `@devcontainers/cli@0.87.0` carries the arbitrary `customizations.aileron`
  namespace into the `devcontainer.metadata` label in full fidelity.
- Composing the real gh Feature onto an Alpine substrate that ends in
  `USER agent` + a custom entrypoint installs `gh` and preserves the user,
  entrypoint, and cmd.
- `devcontainer build --platform linux/arm64` produces an arm64 image with the
  label intact.
