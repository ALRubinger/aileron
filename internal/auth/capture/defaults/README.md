# Built-in capture defaults (empty by design)

This directory is the trusted built-in (shipped) layer of the capture
descriptor config convention (built-in -> unit-derived -> user). Each
`.yaml` file here would declare one tool's credential-acquisition knowledge
as data.

It is intentionally empty of `.yaml` files. `gh` used to ship here as
`gh.yaml`; as of #1323 its complete credential story (acquisition + sealing)
lives in its devcontainer Feature CLI unit under
`images/sandbox-features/gh/devcontainer-feature.json`
(`customizations.aileron.cli`), and the host resolves it at runtime by
inspecting the sandbox image's `devcontainer.metadata` label
(`internal/cli/unitloader`). No `gh` capture descriptor is embedded in core
anymore.

This README is a placeholder so the directory stays non-empty (the package
embeds `all:defaults`; an empty directory would not embed). The loader
filters to `.yaml`, so this file is skipped and the built-in capture layer
loads cleanly to zero descriptors. Adding a new built-in tool is a new
`.yaml` file here, never new Go.
